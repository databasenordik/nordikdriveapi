//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/lib/pq"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/internal/activity"
	"nordik-drive-api/internal/audit"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/community"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/logs"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/session"
)

// Run the checked-in psql migration, including its gates and generated sequence
// grants, on one connection in the already-validated disposable local database.
func applyOwnerMigration(db *gorm.DB, filename string, variables map[string]string) error {
	content, err := os.ReadFile("../../migrations/" + filename)
	if err != nil {
		return err
	}
	var lines []string
	active, parent, condition := true, true, true
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, `\set `):
			continue
		case strings.HasPrefix(trimmed, `\if :`):
			key := strings.TrimPrefix(trimmed, `\if :`)
			value, found := variables[key]
			if !found || (value != "true" && value != "false") {
				return errors.New("missing explicit migration gate")
			}
			parent = active
			condition = value == "true"
			active = parent && condition
			continue
		case trimmed == `\else`:
			active = parent && !condition
			continue
		case trimmed == `\endif`:
			active = parent
			continue
		case strings.HasPrefix(trimmed, `\echo `):
			continue
		case strings.HasPrefix(trimmed, `\quit `):
			if active {
				return errors.New("migration gate refused")
			}
			continue
		}
		if active {
			lines = append(lines, line)
		}
	}
	source := strings.Join(lines, "\n")
	for key, value := range variables {
		source = strings.ReplaceAll(source, `:"`+key+`"`, pgx.Identifier{value}.Sanitize())
		source = strings.ReplaceAll(source, `:'`+key+`'`, `'`+strings.ReplaceAll(value, `'`, `''`)+`'`)
	}
	pool, err := db.DB()
	if err != nil {
		return err
	}
	connection, err := pool.Conn(context.Background())
	if err != nil {
		return err
	}
	defer connection.Close()
	defer connection.ExecContext(context.Background(), "ROLLBACK")
	chunks := strings.Split(source, `\gexec`)
	for index, chunk := range chunks {
		if index == len(chunks)-1 {
			if strings.TrimSpace(chunk) != "" {
				_, err = connection.ExecContext(context.Background(), chunk)
			}
			return err
		}
		at := strings.LastIndex(chunk, "SELECT format(")
		if at < 0 {
			return errors.New("unrecognized generated migration grant")
		}
		if strings.TrimSpace(chunk[:at]) != "" {
			if _, err := connection.ExecContext(context.Background(), chunk[:at]); err != nil {
				return err
			}
		}
		rows, err := connection.QueryContext(context.Background(), chunk[at:])
		if err != nil {
			return err
		}
		generated := []string{}
		for rows.Next() {
			var command string
			if err := rows.Scan(&command); err != nil {
				rows.Close()
				return err
			}
			generated = append(generated, command)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, command := range generated {
			if _, err := connection.ExecContext(context.Background(), command); err != nil {
				return err
			}
		}
	}
	return nil
}

func ownerRoleDB(t *testing.T, admin *gorm.DB, role, path string) *gorm.DB {
	t.Helper()
	var databaseName string
	if err := admin.Raw("SELECT current_database()").Scan(&databaseName).Error; err != nil {
		t.Fatal(err)
	}
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	connection.Path = "/" + databaseName
	connection.RawQuery = "sslmode=disable&connect_timeout=5"
	cfg, err := pgx.ParseConfig(connection.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = path
	pool := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}))
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

type ownerDirectory struct {
	profiles map[int64]identityclient.Profile
	fail     bool
}

func (d *ownerDirectory) Profiles(_ context.Context, ids []int64, email bool) (map[int64]identityclient.Profile, error) {
	if d.fail {
		return nil, errors.New("Identity unavailable")
	}
	if email {
		return nil, errors.New("Audit may not request recipient email")
	}
	result := map[int64]identityclient.Profile{}
	for _, id := range ids {
		if p, ok := d.profiles[id]; ok {
			p.Email = ""
			result[id] = p
		}
	}
	return result, nil
}
func (d *ownerDirectory) Principals(context.Context, []int64) (map[int64]identityclient.Principal, error) {
	return nil, errors.New("Audit may not request principals")
}
func (d *ownerDirectory) Search(_ context.Context, query, mode, role string) ([]int64, error) {
	if d.fail {
		return nil, errors.New("Identity unavailable")
	}
	if mode != "parts" || role != "" {
		return nil, errors.New("activity search semantics changed")
	}
	switch query {
	case "Aur", "Riv_er":
		return []int64{52}, nil
	case "%", "_":
		return []int64{52, 53}, nil
	case "Aurora Riv_er", "absent":
		return []int64{}, nil
	}
	return nil, errors.New("unplanned name-search fixture")
}

type databaseAuditSource struct {
	db        *gorm.DB
	auditPool *sql.DB
	loseAck   bool
}

func (s *databaseAuditSource) Read(ctx context.Context, id string) (auditoutbox.SourceRecord, error) {
	if s.auditPool.Stats().InUse != 0 {
		return auditoutbox.SourceRecord{}, errors.New("owner read attempted during audit transaction")
	}
	var row auditoutbox.Record
	if err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		return auditoutbox.SourceRecord{}, err
	}
	result := auditoutbox.SourceRecord{Receipt: auditoutbox.Receipt{ID: row.ID, Fingerprint: row.Fingerprint, LogID: row.LogID}}
	if row.State == "pending" {
		var event activity.Event
		if err := json.Unmarshal([]byte(row.Payload), &event); err != nil {
			return result, err
		}
		result.Event = &event
	}
	return result, nil
}
func (s *databaseAuditSource) Ack(ctx context.Context, receipt auditoutbox.Receipt) error {
	if s.auditPool.Stats().InUse != 0 {
		return errors.New("owner ack attempted before audit commit")
	}
	if s.loseAck {
		return errors.New("simulated lost acknowledgment")
	}
	return s.db.WithContext(ctx).Model(&auditoutbox.Record{}).Where("id = ? AND fingerprint = ?", receipt.ID, receipt.Fingerprint).Updates(map[string]any{"state": "ingested", "payload": "", "log_id": receipt.LogID}).Error
}

func TestPostgresIdentityAuditRelocationPermissionsAndHistory(t *testing.T) {
	db := notificationPostgres(t)
	if err := db.AutoMigrate(&auth.Auth{}, &auth.Role{}, &auth.OTP{}, &community.Community{}, &logs.SystemLog{}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"file_edit_request", "form_submissions", "support_requests", "support_schedule_audit_logs"} {
		if err := db.Exec("CREATE TABLE " + table + " (id bigserial PRIMARY KEY)").Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, migration := range []string{"003_notifications.up.sql", "004_signup_cleanup.up.sql"} {
		if err := applyOwnerMigration(db, migration, nil); err != nil {
			t.Fatal(migration, err)
		}
	}
	user := auth.Auth{ID: 52, FirstName: "Aurora", LastName: "Riv_er", Email: "existing@example.test", Password: "existing-unchanged-hash", Role: "User", Community: pq.StringArray{"North"}, CreatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	second := auth.Auth{ID: 53, FirstName: "", LastName: "", Email: "blank@example.test", Password: "existing-blank-hash", Role: "User"}
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("SELECT setval(pg_get_serial_sequence('public.users','id'),53,true)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&community.Community{Name: "North", Approved: true}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		action := "SET NULL"
		if i%2 == 0 {
			action = "CASCADE"
		}
		if err := db.Exec(fmt.Sprintf("CREATE TABLE dependency_%02d (id int PRIMARY KEY,user_id bigint REFERENCES public.users(id) ON DELETE %s)", i, action)).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(fmt.Sprintf("INSERT INTO dependency_%02d VALUES (1,52)", i)).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("CREATE SCHEMA nia; CREATE VIEW nia.community_view AS SELECT community_name FROM public.communities").Error; err != nil {
		t.Fatal(err)
	}
	prefix := "m4_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	roles := map[string]string{"identity_user": prefix + "_i", "audit_user": prefix + "_a", "legacy_user": prefix + "_l", "worker_user": prefix + "_w", "nia_user": prefix + "_n"}
	for _, role := range roles {
		if err := db.Exec("CREATE ROLE " + pgx.Identifier{role}.Sanitize() + " NOLOGIN").Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, role := range roles {
			_ = db.Exec("DROP OWNED BY " + pgx.Identifier{role}.Sanitize()).Error
			_ = db.Exec("DROP ROLE " + pgx.Identifier{role}.Sanitize()).Error
		}
	})
	for _, key := range []string{"legacy_user", "worker_user"} {
		role := pgx.Identifier{roles[key]}.Sanitize()
		if err := db.Exec("GRANT USAGE ON SCHEMA public TO " + role + "; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO " + role + "; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO " + role).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("GRANT USAGE ON SCHEMA nia TO " + pgx.Identifier{roles["nia_user"]}.Sanitize() + "; GRANT SELECT ON nia.community_view TO " + pgx.Identifier{roles["nia_user"]}.Sanitize()).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := applyOwnerMigration(db, "005_identity_audit_foundations.up.sql", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyOwnerMigration(db, "005_identity_audit_compatibility_grants.sql", roles); err != nil {
		t.Fatal(err)
	}
	if err := applyOwnerMigration(db, "005_audit_compatibility_grants.sql", roles); err != nil {
		t.Fatal(err)
	}
	uid := uint(52)
	missing := uint(998)
	for index, id := range []*uint{&uid, &missing, nil} {
		row := logs.SystemLog{ID: uint(5000 + index), Level: "INFO", Service: "auth", UserID: id, Action: "LOGIN", Message: "Historical activity", CreatedAt: time.Now().UTC().Add(-time.Minute), Communities: pq.StringArray{"North"}}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("SELECT setval(pg_get_serial_sequence('public.logs','id'),5002,true)").Error; err != nil {
		t.Fatal(err)
	}
	compatibilityDB := ownerRoleDB(t, db, roles["identity_user"], "identity,public")
	if err := (&logs.LogService{DB: compatibilityDB}).Log(logs.SystemLog{
		Level: "INFO", Service: "auth", UserID: &uid, Action: "LOGIN",
		Message: "Identity compatibility activity", Communities: pq.StringArray{"North"},
	}, map[string]any{"phase": "identity_compatibility"}); err != nil {
		t.Fatal("Identity cannot preserve activity writes before Audit extraction", err)
	}
	var generatedLogID uint
	if err := db.Raw("SELECT id FROM public.logs WHERE message = ?", "Identity compatibility activity").Scan(&generatedLogID).Error; err != nil || generatedLogID <= 5002 {
		t.Fatal("compatibility activity did not receive its existing sequence ID", generatedLogID, err)
	}
	if err := compatibilityDB.Exec("SELECT message FROM public.logs").Error; err == nil {
		t.Fatal("Identity compatibility writer can read activity content")
	}
	type history struct {
		rows  []logs.LogRow
		aggs  logs.LogAggregates
		total int64
		pages int
	}
	queries := []*string{nil}
	for _, q := range []string{"Aur", "Riv_er", "Aurora Riv_er", "%", "_", "absent"} {
		value := q
		queries = append(queries, &value)
	}
	baseline := []history{}
	for _, query := range queries {
		rows, aggs, total, pages, err := (&logs.LogService{DB: db}).GetLogs(logs.LogFilterInput{Search: query, Page: 1, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		baseline = append(baseline, history{rows, aggs, total, pages})
	}
	var usersOID, communitiesOID, usersSequenceOID, logsOID uint32
	for query, destination := range map[string]*uint32{"SELECT 'public.users'::regclass::oid": &usersOID, "SELECT 'public.communities'::regclass::oid": &communitiesOID, "SELECT pg_get_serial_sequence('public.users','id')::regclass::oid": &usersSequenceOID, "SELECT 'public.logs'::regclass::oid": &logsOID} {
		if err := db.Raw(query).Scan(destination).Error; err != nil {
			t.Fatal(err)
		}
	}
	type dependency struct {
		OID    uint32
		Delete string `gorm:"column:confdeltype"`
	}
	var dependencies []dependency
	if err := db.Raw("SELECT oid,confdeltype::text FROM pg_constraint WHERE contype='f' AND confrelid='public.users'::regclass ORDER BY oid").Scan(&dependencies).Error; err != nil || len(dependencies) != 30 {
		t.Fatal("fixture user dependencies", len(dependencies), err)
	}
	encodedKey, _ := json.Marshal(map[string]string{"1": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))})
	codec, err := notificationcontract.NewCodec(string(encodedKey), "1")
	if err != nil {
		t.Fatal(err)
	}
	notifications := &outbox.Writer{Codec: codec}
	for _, state := range []string{"pending", "accepted", "provider_accepted", "unknown"} {
		if err := notifications.Send(db, "identity", "preserve:"+state, []string{"synthetic@example.test"}, "Synthetic email", "Protected synthetic content"); err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&outbox.Record{}).Where("event_key = ?", "preserve:"+state+":synthetic@example.test").Updates(map[string]any{"state": state, "attempts": 2, "delivery_id": uuid.NewString()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var before []outbox.Record
	if err := db.Order("id").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	cookie := fixtureCookie(t, 52)
	if err := applyOwnerMigration(db, "006_identity_relocate.sql", roles); err == nil {
		t.Fatal("relocation passed without an explicit compatibility gate")
	}
	gate := map[string]string{}
	for k, v := range roles {
		gate[k] = v
	}
	gate["compatibility_verified"] = "true"
	if err := applyOwnerMigration(db, "006_identity_relocate.sql", gate); err != nil {
		t.Fatal(err)
	}
	if err := applyOwnerMigration(db, "007_audit_relocate.sql", gate); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]uint32{"SELECT 'identity.users'::regclass::oid": usersOID, "SELECT 'identity.communities'::regclass::oid": communitiesOID, "SELECT pg_get_serial_sequence('identity.users','id')::regclass::oid": usersSequenceOID, "SELECT 'audit.logs'::regclass::oid": logsOID} {
		var got uint32
		if err := db.Raw(query).Scan(&got).Error; err != nil || got != want {
			t.Fatal("physical relation/sequence changed", query, err)
		}
	}
	var afterDependencies []dependency
	if err := db.Raw("SELECT oid,confdeltype::text FROM pg_constraint WHERE contype='f' AND confrelid='identity.users'::regclass ORDER BY oid").Scan(&afterDependencies).Error; err != nil || !reflect.DeepEqual(dependencies, afterDependencies) {
		t.Fatal("user foreign keys or deletion behavior changed", err)
	}
	var after []outbox.Record
	if err := db.Table("identity.notification_outbox").Order("id").Find(&after).Error; err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("notification outbox state/ciphertext/identifiers changed", err)
	}
	var count int64
	if err := db.Table("public.notification_outbox").Where("producer='identity'").Count(&count).Error; err != nil || count != 0 {
		t.Fatal("identity intents remain at legacy owner", err)
	}
	claims, err := session.Validate(cookie.Value, os.Getenv("JWT_SECRET"))
	if err != nil || claims.UserID != 52 {
		t.Fatal("existing cookie changed", err)
	}
	var existing auth.Auth
	if err := db.Table("identity.users").First(&existing, "id = ?", 52).Error; err != nil || existing.Password != user.Password || existing.CreatedAt.Sub(user.CreatedAt).Abs() > time.Microsecond {
		t.Fatal("existing user changed", err)
	}
	identityDB := ownerRoleDB(t, db, roles["identity_user"], "identity,public")
	auditDB := ownerRoleDB(t, db, roles["audit_user"], "audit,public,identity")
	legacyDB := ownerRoleDB(t, db, roles["legacy_user"], "public")
	workerDB := ownerRoleDB(t, db, roles["worker_user"], "public")
	niaDB := ownerRoleDB(t, db, roles["nia_user"], "nia")
	directory := &ownerDirectory{profiles: map[int64]identityclient.Profile{52: {ID: 52, Firstname: user.FirstName, Lastname: user.LastName}, 53: {ID: 53}}}
	for index, query := range queries {
		rows, aggs, total, pages, err := (&logs.LogService{DB: auditDB, Directory: directory}).GetLogs(logs.LogFilterInput{Search: query, Page: 1, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		got := history{rows, aggs, total, pages}
		// Existing aggregates order only by count. PostgreSQL does not promise an
		// order between equal counts, so compare those entries by their stable ID.
		for _, items := range [][]logs.PersonAggItem{got.aggs.ByPerson, baseline[index].aggs.ByPerson} {
			sort.Slice(items, func(i, j int) bool {
				left, right := uint(0), uint(0)
				if items[i].UserID != nil {
					left = *items[i].UserID
				}
				if items[j].UserID != nil {
					right = *items[j].UserID
				}
				return left < right
			})
		}
		if !reflect.DeepEqual(got, baseline[index]) {
			t.Fatalf("activity search/page/aggregate parity differs for query %v", query)
		}
	}
	directory.fail = true
	if _, _, _, _, err := (&logs.LogService{DB: auditDB, Directory: directory}).GetLogs(logs.LogFilterInput{Search: queries[1]}); err == nil || !strings.Contains(err.Error(), "Identity unavailable") {
		t.Fatal("owner failure became partial success", err)
	}
	directory.fail = false
	if err := applyOwnerMigration(db, "008_identity_audit_revoke_transitional.sql", gate); err == nil {
		t.Fatal("grant removal passed without rollback proof")
	}
	gate["compatible_rollback_verified"] = "true"
	if err := applyOwnerMigration(db, "008_identity_audit_revoke_transitional.sql", gate); err != nil {
		t.Fatal(err)
	}
	var retainedLogIDRead bool
	if err := db.Raw("SELECT has_column_privilege(?, 'audit.logs', 'id', 'SELECT')", roles["identity_user"]).Scan(&retainedLogIDRead).Error; err != nil || retainedLogIDRead {
		t.Fatal("transitional log ID column grant survived final ownership revocation", err)
	}
	for _, test := range []struct {
		db      *gorm.DB
		query   string
		allowed bool
	}{{identityDB, "SELECT id FROM identity.users", true}, {identityDB, "SELECT id FROM identity.notification_outbox", true}, {identityDB, "SELECT id FROM audit.logs", false}, {auditDB, "SELECT id FROM audit.logs", true}, {auditDB, "SELECT id FROM identity.users", false}, {auditDB, "SELECT id FROM identity.notification_outbox", false}, {legacyDB, "SELECT id FROM identity.users", false}, {legacyDB, "SELECT id FROM audit.logs", false}, {workerDB, "SELECT id FROM identity.users", false}, {workerDB, "SELECT id FROM identity.signup_otp_challenges", false}, {workerDB, "SELECT id FROM identity.notification_outbox", false}, {niaDB, "SELECT community_name FROM nia.community_view", true}} {
		err := test.db.Exec(test.query).Error
		if (err == nil) != test.allowed {
			t.Fatalf("unexpected restricted permission for %s: %v", test.query, err)
		}
	}
	if err := legacyDB.Exec("INSERT INTO dependency_00 VALUES (2,52)").Error; err != nil {
		t.Fatal("FK checks require an application users-table grant", err)
	}
	created, err := (&auth.AuthService{DB: identityDB, Outbox: notifications, Audit: &auditoutbox.Writer{Producer: "identity"}}).CreateUser(auth.Auth{FirstName: "New", LastName: "Owner", Email: "new@example.test", Password: "synthetic-hash"})
	if err != nil || created.ID != 54 {
		t.Fatal("owner mutation or moved sequence failed", err)
	}
	auditWriter := &auditoutbox.Writer{Producer: "legacy"}
	if err := legacyDB.Transaction(func(tx *gorm.DB) error {
		return auditWriter.Put(tx, activity.Log{Level: "WARN", Service: "file", UserID: &uid, Action: "DELETE_FILE", Message: "File deleted : synthetic.csv"}, map[string]any{"password": "secret-canary", "signup_verification_proof": "proof-canary"})
	}); err != nil {
		t.Fatal(err)
	}
	var event auditoutbox.Record
	if err := legacyDB.First(&event).Error; err != nil {
		t.Fatal(err)
	}
	if err := legacyDB.Model(&auditoutbox.Record{}).Where("id = ?", event.ID).Update("payload", "tampered").Error; err == nil {
		t.Fatal("producer event was mutable")
	}
	auditPool, _ := auditDB.DB()
	source := &databaseAuditSource{db: legacyDB, auditPool: auditPool, loseAck: true}
	ingestion := &audit.Service{DB: auditDB, Sources: []audit.OwnerSource{{Producer: "legacy", Source: source}}}
	if ingestion.Ingest(context.Background(), event.ID) == nil {
		t.Fatal("lost acknowledgment hidden")
	}
	source.loseAck = false
	for range 2 {
		if err := ingestion.Ingest(context.Background(), event.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := auditDB.Table("logs").Where("action='DELETE_FILE'").Count(&count).Error; err != nil || count != 1 {
		t.Fatal("duplicate activity after lost acknowledgment", err)
	}
	if err := legacyDB.First(&event, "id = ?", event.ID).Error; err != nil || event.Payload != "" || event.State != "ingested" {
		t.Fatal("acknowledged payload not cleared", err)
	}
	for _, table := range []string{"public.user_roles", "identity.user_roles", "public.access", "identity.access"} {
		var exists bool
		if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", table).Scan(&exists).Error; err != nil || exists {
			t.Fatal("absent membership table was created", err)
		}
	}
	if err := identityDB.Delete(&auth.Auth{}, 52).Error; err != nil {
		t.Fatal("existing user deletion semantics changed", err)
	}
	for i := 0; i < 30; i++ {
		if err := db.Table(fmt.Sprintf("dependency_%02d", i)).Where("id=1").Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if (count == 0) != (i%2 == 0) {
			t.Fatal("CASCADE/SET NULL behavior changed")
		}
	}
}
