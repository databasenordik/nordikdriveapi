package file

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"gorm.io/datatypes"
	"nordik-drive-api/internal/activity"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
)

type fileDirectory struct {
	pool     *sql.DB
	profiles map[int64]identityclient.Profile
	roles    map[int64]identityclient.Principal
	calls    int
	fail     bool
}

func (d *fileDirectory) check() error {
	d.calls++
	if d.pool.Stats().InUse != 0 {
		return errors.New("owner RPC inside business transaction")
	}
	if d.fail {
		return errors.New("Identity unavailable")
	}
	return nil
}
func (d *fileDirectory) Profiles(_ context.Context, ids []int64, email bool) (map[int64]identityclient.Profile, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	result := map[int64]identityclient.Profile{}
	for _, id := range ids {
		if p, ok := d.profiles[id]; ok {
			if !email {
				p.Email = ""
			}
			result[id] = p
		}
	}
	return result, nil
}
func (d *fileDirectory) Principals(_ context.Context, ids []int64) (map[int64]identityclient.Principal, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	result := map[int64]identityclient.Principal{}
	for _, id := range ids {
		if p, ok := d.roles[id]; ok {
			result[id] = p
		}
	}
	return result, nil
}
func (d *fileDirectory) Search(context.Context, string, string, string) ([]int64, error) {
	return nil, errors.New("unexpected profile search")
}
func ownerFileFixture(t *testing.T) (*FileService, *fileDirectory) {
	t.Helper()
	db := newTestDB(t)
	pool, _ := db.DB()
	d := &fileDirectory{pool: pool, profiles: map[int64]identityclient.Profile{1: {ID: 1, Firstname: "One", Lastname: "User", Email: "one@example.test"}, 2: {ID: 2, Firstname: "Two", Lastname: "User", Email: "two@example.test"}}, roles: map[int64]identityclient.Principal{1: {ID: 1, Role: "User"}, 2: {ID: 2, Role: "Admin"}}}
	if err := db.Migrator().RenameTable("users", "identity_owned_users"); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	codec, err := notificationcontract.NewCodec(`{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	return &FileService{DB: db, Directory: d, Outbox: &outbox.Writer{Codec: codec}}, d
}
func TestFilesOwnerQueriesWorkWithoutUsersTable(t *testing.T) {
	service, directory := ownerFileFixture(t)
	files := []File{{Filename: "public.csv", InsertedBy: 1, Version: 1}, {Filename: "private.csv", InsertedBy: 2, Private: true, Version: 1}, {Filename: "deleted.csv", InsertedBy: 1, IsDelete: true, Version: 1}, {Filename: "missing-uploader.csv", InsertedBy: 998, Version: 1}}
	if err := service.DB.Create(&files).Error; err != nil {
		t.Fatal(err)
	}
	role, err := service.GetUserRole(2)
	if err != nil || role != "Admin" {
		t.Fatal("principal role changed", err)
	}
	all, err := service.GetAllFiles(2, role)
	if err != nil || len(all) != 4 {
		t.Fatal("admin listing changed", err)
	}
	public, err := service.GetAllFiles(1, "User")
	if err != nil || len(public) != 2 {
		t.Fatal("public-file authorization changed", err)
	}
	accesses := []FileAccess{{UserID: 1, FileID: files[1].ID}, {UserID: 998, FileID: files[1].ID}}
	if err := service.DB.Create(&accesses).Error; err != nil {
		t.Fatal(err)
	}
	granted, err := service.GetAllFiles(1, "User")
	if err != nil || len(granted) != 3 {
		t.Fatal("private-file grant changed", err)
	}
	access, err := service.GetFileAccess(strconv.FormatUint(uint64(files[1].ID), 10))
	if err != nil || len(access) != 1 || access[0].FirstName != "One" {
		t.Fatal("inner-join missing-profile semantics changed", err)
	}
	version := FileVersion{FileID: files[0].ID, Filename: files[0].Filename, Version: 1, InsertedBy: 1}
	if err := service.DB.Create(&version).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.DB.Create(&FileData{FileID: files[0].ID, Version: 2, InsertedBy: 2, RowData: datatypes.JSON(`{"value":"synthetic"}`)}).Error; err != nil {
		t.Fatal(err)
	}
	history, err := service.GetFileHistory(strconv.FormatUint(uint64(files[0].ID), 10))
	if err != nil || len(history) != 2 {
		t.Fatal("version history changed", err)
	}
	names := map[int]string{}
	for _, row := range history {
		names[row.Version] = row.Firstname
	}
	if names[1] != "One" || names[2] != "Two" {
		t.Fatal("version aggregate enrichment changed")
	}
	requests := []FileEditRequest{{UserID: 1, FileID: files[0].ID, Status: "pending"}, {UserID: 998, FileID: files[0].ID, Status: "pending"}}
	if err := service.DB.Create(&requests).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := service.GetEditRequests(nil, nil)
	if err != nil || len(pending) != 1 || pending[0].Firstname != "One" {
		t.Fatal("review-list owner enrichment changed", err)
	}
	detail := FileEditRequestDetails{RequestID: requests[0].RequestID, FileID: files[0].ID, Filename: files[0].Filename, FieldName: "value", NewValue: "synthetic"}
	if err := service.DB.Create(&detail).Error; err != nil {
		t.Fatal(err)
	}
	before := directory.calls
	if err := service.ReviewEditRequest(requests[0].RequestID, "completed", "reviewed", []FileEditRequestDetails{{ID: detail.ID, NewValue: "synthetic", Status: "rejected"}}, 2); err != nil {
		t.Fatal(err)
	}
	if directory.calls-before != 1 {
		t.Fatal("review did not batch recipient before transaction")
	}
	var intent outbox.Record
	if err := service.DB.First(&intent).Error; err != nil {
		t.Fatal(err)
	}
	var command notificationcontract.Command
	if json.Unmarshal([]byte(intent.Command), &command) != nil {
		t.Fatal("notification missing")
	}
	payload, err := service.Outbox.Codec.Open(command)
	if err != nil || payload.To != "one@example.test" {
		t.Fatal("review recipient changed", err)
	}
	directory.fail = true
	if _, err := service.GetAllFiles(2, "Admin"); err == nil {
		t.Fatal("owner outage returned partial file success")
	}
	if _, err := service.GetUserRole(2); err == nil {
		t.Fatal("owner outage fell back to SQL principals")
	}
}
func TestFilesAuditFailureRollsBackBusinessChanges(t *testing.T) {
	service, _ := ownerFileFixture(t)
	service.Audit = &auditoutbox.Writer{Producer: "legacy"}
	service.DB = service.DB.WithContext(activity.WithActor(context.Background(), 1, []string{"North"}))
	file := File{Filename: "atomic.csv", InsertedBy: 1, Version: 1}
	if err := service.DB.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatUint(uint64(file.ID), 10)
	if _, err := service.DeleteFile(id); err == nil {
		t.Fatal("mutation committed without durable audit intent")
	}
	var unchanged File
	if err := service.DB.First(&unchanged, file.ID).Error; err != nil || unchanged.IsDelete {
		t.Fatal("audit failure did not roll back soft delete")
	}
	if err := service.DB.AutoMigrate(&auditoutbox.Record{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DeleteFile(id); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResetFile(id); err != nil {
		t.Fatal(err)
	}
	var events []auditoutbox.Record
	if err := service.DB.Order("created_at").Find(&events).Error; err != nil || len(events) != 2 {
		t.Fatal("mutation audit records missing", err)
	}
	for i, row := range events {
		var event activity.Event
		if json.Unmarshal([]byte(row.Payload), &event) != nil || event.Log.UserID == nil || *event.Log.UserID != 1 || event.Log.Filename == nil || *event.Log.Filename != "atomic.csv" {
			t.Fatal("activity fields changed")
		}
		expected := []string{"DELETE_FILE", "RESTORE_FILE"}[i]
		if event.Log.Action != expected {
			t.Fatal("activity action changed")
		}
	}
}
