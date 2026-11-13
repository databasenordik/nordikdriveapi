package supportrequest

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
)

type requestDirectory struct {
	pool  *sql.DB
	users map[int64]SupportRequestUserRef
	fail  bool
}

func (d *requestDirectory) check() error {
	if d.pool.Stats().InUse != 0 {
		return errors.New("owner RPC in business transaction")
	}
	if d.fail {
		return errors.New("Identity unavailable")
	}
	return nil
}
func (d *requestDirectory) Profiles(_ context.Context, ids []int64, email bool) (map[int64]identityclient.Profile, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	result := map[int64]identityclient.Profile{}
	for _, id := range ids {
		if u, ok := d.users[id]; ok {
			p := identityclient.Profile{ID: id, Firstname: u.FirstName, Lastname: u.LastName}
			if email {
				p.Email = u.Email
			}
			result[id] = p
		}
	}
	return result, nil
}
func (d *requestDirectory) Principals(_ context.Context, ids []int64) (map[int64]identityclient.Principal, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	result := map[int64]identityclient.Principal{}
	for _, id := range ids {
		if u, ok := d.users[id]; ok {
			result[id] = identityclient.Principal{ID: id, Role: u.Role}
		}
	}
	return result, nil
}
func (d *requestDirectory) Search(context.Context, string, string, string) ([]int64, error) {
	return nil, errors.New("unexpected search")
}
func TestSupportRequestUsesOwnerProfilesAndPreservesAdminChecks(t *testing.T) {
	service, _ := newTestService(t)
	pool, _ := service.DB.DB()
	directory := &requestDirectory{pool: pool, users: map[int64]SupportRequestUserRef{}}
	var users []SupportRequestUserRef
	if err := service.DB.Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		directory.users[int64(user.ID)] = user
	}
	if err := service.DB.Migrator().RenameTable("users", "identity_owned_users"); err != nil {
		t.Fatal(err)
	}
	if err := service.DB.AutoMigrate(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	codec, err := notificationcontract.NewCodec(`{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	service.Directory = directory
	service.Outbox = &outbox.Writer{Codec: codec}
	service.Mailer = nil
	request := &CreateSupportRequestRequest{RequestType: RequestTypeQuestion, Subject: "Synthetic question", Message: "Synthetic message"}
	created, err := service.Create(request, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored SupportRequest
	if err := service.DB.First(&stored, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RequesterName != "User One" || stored.RequesterEmail != "user1@example.com" {
		t.Fatal("requester profile/recipient changed", err)
	}
	mine, err := service.ListMine(1, 1, 10)
	if err != nil || mine.TotalItems != 1 {
		t.Fatal("request ownership changed", err)
	}
	admin := directory.users[99]
	admin.Role = "aDmIn"
	directory.users[99] = admin
	listed, err := service.ListForAdmin(99, 1, 10)
	if err != nil || listed.TotalItems != 1 {
		t.Fatal("case-insensitive admin role changed", err)
	}
	if _, err := service.ListForAdmin(1, 1, 10); !errors.Is(err, ErrSupportRequestForbidden) {
		t.Fatal("non-admin permission changed", err)
	}
	if _, err := service.ListForAdmin(998, 1, 10); !errors.Is(err, ErrSupportRequestForbidden) {
		t.Fatal("missing principal granted access", err)
	}
	directory.fail = true
	if _, err := service.Create(request, 1, nil); err == nil {
		t.Fatal("owner outage allowed mutation")
	}
	var count int64
	if err := service.DB.Model(&SupportRequest{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("failed owner call committed a request", err)
	}
	if _, err := service.ListForAdmin(99, 1, 10); err == nil {
		t.Fatal("owner outage fell back to SQL authorization")
	}
}
