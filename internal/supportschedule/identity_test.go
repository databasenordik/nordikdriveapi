package supportschedule

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/outbox"
)

type supportDirectory struct {
	pool  *sql.DB
	users map[uint]SupportUser
	fail  bool
	calls int
}

func (d *supportDirectory) check(ctx context.Context) error {
	d.calls++
	if d.fail {
		return errors.New("identity unavailable")
	}
	if d.pool.Stats().InUse != 0 {
		return errors.New("owner RPC attempted while a business transaction held a connection")
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	connection, err := d.pool.Conn(ctx)
	if err != nil {
		return errors.New("owner RPC attempted while business transaction held the only connection")
	}
	return connection.Close()
}
func (d *supportDirectory) Profiles(ctx context.Context, ids []int64, email bool) (map[int64]identityclient.Profile, error) {
	if err := d.check(ctx); err != nil {
		return nil, err
	}
	result := map[int64]identityclient.Profile{}
	for _, id := range ids {
		if user, found := d.users[uint(id)]; found {
			p := identityclient.Profile{ID: id, Firstname: user.FirstName, Lastname: user.LastName}
			if email {
				p.Email = user.Email
			}
			result[id] = p
		}
	}
	return result, nil
}
func (d *supportDirectory) Principals(ctx context.Context, ids []int64) (map[int64]identityclient.Principal, error) {
	if err := d.check(ctx); err != nil {
		return nil, err
	}
	result := map[int64]identityclient.Principal{}
	for _, id := range ids {
		if user, found := d.users[uint(id)]; found {
			result[id] = identityclient.Principal{ID: id, Role: user.Role}
		}
	}
	return result, nil
}
func (d *supportDirectory) Search(ctx context.Context, query, mode, role string) ([]int64, error) {
	if err := d.check(ctx); err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, id := range []uint{1, 2, 3, 4} {
		if user, found := d.users[id]; found && (role == "" || strings.EqualFold(user.Role, role)) {
			ids = append(ids, int64(id))
		}
	}
	return ids, nil
}

func ownerScheduleFixture(t *testing.T) (scheduleFixture, *supportDirectory) {
	t.Helper()
	fixture := newScheduleFixture(t)
	var users []SupportUser
	if err := fixture.service.DB.Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	pool, err := fixture.service.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(2)
	directory := &supportDirectory{pool: pool, users: map[uint]SupportUser{}}
	for _, user := range users {
		directory.users[user.ID] = user
	}
	if err := fixture.service.DB.Exec("ALTER TABLE users RENAME TO identity_owned_users").Error; err != nil {
		t.Fatal(err)
	}
	fixture.service.Directory = directory
	return fixture, directory
}

func TestSupportWorksWithoutUserTableAccessAndBeforeTransactions(t *testing.T) {
	f, directory := ownerScheduleFixture(t)
	start := f.now.Add(4 * time.Hour)
	request, err := f.service.CreateCall(f.survivor, CreateCallInput{RequestedStaffID: &f.staffB, ScheduledStart: start.Format(time.RFC3339), DurationMinutes: 30, Subject: "Owner API parity", Message: "Synthetic support request"})
	if err != nil {
		t.Fatal(err)
	}
	if request.RequestedBy.FirstName != "Sam" || request.AssignedStaff == nil || request.AssignedStaff.FirstName != "Blair" {
		t.Fatal("request profile enrichment changed")
	}
	request, err = f.service.DecideRequest(f.staffB, request.ID, RequestDecisionInput{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != RequestStatusApproved {
		t.Fatal("approval changed")
	}
	call, err := f.service.ReassignCall(f.manager, request.Call.ID, ReassignInput{UserID: f.staffA, Reason: "Synthetic reassignment"})
	if err != nil {
		t.Fatal(err)
	}
	if call.AssignedStaff == nil || call.AssignedStaff.Email != "alex@example.test" {
		t.Fatal("reassignment profile changed")
	}
	if rows, err := f.service.ListRequests(f.survivor, "mine"); err != nil || len(rows) != 1 || rows[0].RequestedBy.ID != f.survivor {
		t.Fatalf("request list missing owner names: %v", err)
	}
	if rows, err := f.service.ListCalls(f.manager, "all"); err != nil || len(rows) != 1 || rows[0].AssignedStaff == nil {
		t.Fatalf("call list missing owner names: %v", err)
	}
	if rows, err := f.service.ListTeam(f.manager); err != nil || len(rows) != 2 || rows[0].User.FirstName == "" {
		t.Fatalf("team list failed: %v", err)
	}
	if _, err := f.service.ListCalendar(f.survivor, 30, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetProfile(f.staffA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ListFairnessStats(f.manager); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ReassignDay(f.manager, start.Format("2006-01-02"), ReassignInput{UserID: f.staffB, Reason: "Synthetic day change"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CancelRequest(f.survivor, request.ID); err != nil {
		t.Fatal(err)
	}
	if directory.calls == 0 {
		t.Fatal("consumer bypassed Identity")
	}
}

func TestSupportNotificationSnapshotAndZoomHostUsePreparedOwnerData(t *testing.T) {
	f, _ := ownerScheduleFixture(t)
	start := f.now.Add(4 * time.Hour)
	request, err := f.service.CreateCall(f.survivor, CreateCallInput{RequestedStaffID: &f.staffB, ScheduledStart: start.Format(time.RFC3339), DurationMinutes: 30, Subject: "Snapshot parity"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.service.prepareIdentity(userScope{Requests: []uint{request.ID}})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Outbox = &outbox.Writer{}
	if err := prepared.DB.Transaction(func(tx *gorm.DB) error {
		if err := prepared.recordAudit(tx, "support_request", &request.ID, "support_request_approve", &f.staffB, map[string]any{}); err != nil {
			return err
		}
		input, err := prepared.meetingInput(tx, request, request.Call, nil)
		if err != nil {
			return err
		}
		if input.HostEmail != "blair@example.test" {
			return errors.New("Zoom host identity changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var record SupportScheduleAuditLog
	if err := prepared.DB.Order("id DESC").First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.NotificationState != "pending" || !strings.Contains(record.NotificationSnapshot, "sam@example.test") || !strings.Contains(record.NotificationSnapshot, "blair@example.test") {
		t.Fatal("durable Support snapshot lost its owner profiles")
	}
}

func TestSupportOwnerFailureIsExplicitAndCannotMutateWorkflow(t *testing.T) {
	f, directory := ownerScheduleFixture(t)
	directory.fail = true
	if _, err := f.service.ListTeam(f.manager); err == nil {
		t.Fatal("owner outage became incomplete success")
	}
	if _, err := f.service.CreateCall(f.survivor, CreateCallInput{}); err == nil {
		t.Fatal("owner outage allowed mutation")
	}
	var count int64
	if err := f.service.DB.Model(&SupportCallRequest{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("failed owner call changed business state")
	}
	directory.fail = false
	if err := f.service.DB.Transaction(func(tx *gorm.DB) error {
		_, err := f.service.identityUsers(tx, []uint{f.staffA})
		if err == nil {
			return errors.New("unprepared transaction made an owner call")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
