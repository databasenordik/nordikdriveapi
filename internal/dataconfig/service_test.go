package dataconfig

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var testDBSeq uint64

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	id := atomic.AddUint64(&testDBSeq, 1)
	dsn := fmt.Sprintf("file:dataconfig_test_%d?mode=memory&cache=shared", id)

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := db.AutoMigrate(&DataConfig{}); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func breakDB(t *testing.T, db *gorm.DB) {
	t.Helper()

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	_ = sqlDB.Close()
}

func insertConfig(t *testing.T, db *gorm.DB, cfg DataConfig) DataConfig {
	t.Helper()

	wantUpdatedAt := cfg.UpdatedAt
	wantIsActive := cfg.IsActive

	if err := db.Create(&cfg).Error; err != nil {
		t.Fatalf("insert config: %v", err)
	}

	// Force zero-value bool to be stored correctly even with default:true tag
	if err := db.Model(&DataConfig{}).
		Where("id = ?", cfg.ID).
		UpdateColumn("is_active", wantIsActive).Error; err != nil {
		t.Fatalf("set is_active: %v", err)
	}
	cfg.IsActive = wantIsActive

	if !wantUpdatedAt.IsZero() {
		if err := db.Model(&DataConfig{}).
			Where("id = ?", cfg.ID).
			UpdateColumn("updated_at", wantUpdatedAt).Error; err != nil {
			t.Fatalf("set updated_at: %v", err)
		}
		cfg.UpdatedAt = wantUpdatedAt
	}

	return cfg
}

func TestDataConfig_TableName(t *testing.T) {
	if got := (DataConfig{}).TableName(); got != "data_config" {
		t.Fatalf("got %q want %q", got, "data_config")
	}
}

func TestDataConfigService_GetByFileNameIfModified_BlankFileName(t *testing.T) {
	svc := &DataConfigService{DB: newTestDB(t)}

	got, err := svc.GetByFileNameIfModified("   ", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Fatalf("expected nil result, got %#v", got)
	}
}

func TestDataConfigService_GetByFileNameIfModified_NotFound(t *testing.T) {
	svc := &DataConfigService{DB: newTestDB(t)}

	got, err := svc.GetByFileNameIfModified("missing.json", nil)
	if err != gorm.ErrRecordNotFound {
		t.Fatalf("err = %v, want %v", err, gorm.ErrRecordNotFound)
	}
	if got != nil {
		t.Fatalf("expected nil result, got %#v", got)
	}
}

func TestDataConfigService_GetByFileNameIfModified_DBError(t *testing.T) {
	db := newTestDB(t)
	svc := &DataConfigService{DB: db}

	breakDB(t, db)

	got, err := svc.GetByFileNameIfModified("test.json", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Fatalf("expected nil result, got %#v", got)
	}
}

func TestDataConfigService_GetByFileNameIfModified_LatestActiveCaseInsensitive(t *testing.T) {
	db := newTestDB(t)
	svc := &DataConfigService{DB: db}

	older := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 2, 21, 10, 0, 0, 0, time.UTC)
	inactiveNewest := time.Date(2026, 2, 22, 10, 0, 0, 0, time.UTC)

	insertConfig(t, db, DataConfig{
		FileID:    1,
		FileName:  "Users.JSON",
		Version:   1,
		Checksum:  "sum-old",
		Config:    datatypes.JSON([]byte(`{"v":1}`)),
		IsActive:  true,
		UpdatedAt: older,
	})

	want := insertConfig(t, db, DataConfig{
		FileID:    2,
		FileName:  "users.json",
		Version:   2,
		Checksum:  "sum-new",
		Config:    datatypes.JSON([]byte(`{"v":2}`)),
		IsActive:  true,
		UpdatedAt: newer,
	})

	insertConfig(t, db, DataConfig{
		FileID:    3,
		FileName:  "users.json",
		Version:   99,
		Checksum:  "sum-inactive",
		Config:    datatypes.JSON([]byte(`{"v":99}`)),
		IsActive:  false,
		UpdatedAt: inactiveNewest,
	})

	got, err := svc.GetByFileNameIfModified("  USERS.json  ", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Config == nil {
		t.Fatal("expected result, got nil")
	}
	if got.NotModified {
		t.Fatal("expected NotModified=false")
	}
	if got.Config.FileID != want.FileID {
		t.Fatalf("file_id = %d want %d", got.Config.FileID, want.FileID)
	}
	if got.Config.Version != want.Version {
		t.Fatalf("version = %d want %d", got.Config.Version, want.Version)
	}
}

func TestDataConfigService_GetByFileNameIfModified_TimestampDoesNotSuppressContent(t *testing.T) {
	db := newTestDB(t)
	svc := &DataConfigService{DB: db}

	updatedAt := time.Date(2026, 2, 23, 15, 4, 5, 0, time.UTC)
	cfg := insertConfig(t, db, DataConfig{
		FileID:    10,
		FileName:  "prefs.json",
		Version:   7,
		Checksum:  "etag-7",
		Config:    datatypes.JSON([]byte(`{"enabled":true}`)),
		IsActive:  true,
		UpdatedAt: updatedAt,
	})

	clientLM := updatedAt
	got, err := svc.GetByFileNameIfModified("prefs.json", &clientLM)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Config == nil {
		t.Fatal("expected result, got nil")
	}
	if got.NotModified {
		t.Fatal("timestamp alone must not suppress current configuration")
	}
	if got.Config.FileID != cfg.FileID {
		t.Fatalf("file_id = %d want %d", got.Config.FileID, cfg.FileID)
	}
}

func TestDataConfigService_GetByFileNameIfModified_ClientOlder_ReturnsModified(t *testing.T) {
	db := newTestDB(t)
	svc := &DataConfigService{DB: db}

	updatedAt := time.Date(2026, 2, 24, 15, 4, 5, 0, time.UTC)
	cfg := insertConfig(t, db, DataConfig{
		FileID:    11,
		FileName:  "prefs.json",
		Version:   8,
		Checksum:  "etag-8",
		Config:    datatypes.JSON([]byte(`{"enabled":false}`)),
		IsActive:  true,
		UpdatedAt: updatedAt,
	})

	clientLM := updatedAt.Add(-time.Minute)

	got, err := svc.GetByFileNameIfModified("prefs.json", &clientLM)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Config == nil {
		t.Fatal("expected result, got nil")
	}
	if got.NotModified {
		t.Fatal("expected NotModified=false")
	}
	if got.Config.FileID != cfg.FileID {
		t.Fatalf("file_id = %d want %d", got.Config.FileID, cfg.FileID)
	}
}
