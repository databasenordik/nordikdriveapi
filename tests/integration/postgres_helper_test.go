//go:build integration

package integration

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Each test owns a random schema in a strictly named local fixture database.
// Application DB_* settings are never used, and missing fixtures fail the suite.
func newPostgresFixture(t testing.TB) *gorm.DB {
	t.Helper()
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || connection == nil || (connection.Scheme != "postgres" && connection.Scheme != "postgresql") {
		t.Fatal("set TEST_DATABASE_URL to the disposable PostgreSQL fixture; see docs/baseline.md")
	}
	host := connection.Hostname()
	if (host != "localhost" && host != "127.0.0.1" && host != "::1") || connection.Path != "/nordik_baseline_test" {
		t.Fatal("PostgreSQL tests require a local nordik_baseline_test database")
	}
	if connection.User == nil || connection.User.Username() != "nordik_test" {
		t.Fatal("PostgreSQL tests require the dedicated nordik_test user")
	}
	query := connection.Query()
	for key := range query {
		if key != "sslmode" && key != "connect_timeout" {
			t.Fatal("fixture URL supports only sslmode and connect_timeout parameters")
		}
	}
	query.Set("connect_timeout", "5")
	connection.RawQuery = query.Encode()
	options := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(connection.String()), options)
	if err != nil {
		t.Fatal("PostgreSQL fixture is unavailable; start docker-compose.test.yml")
	}
	adminPool, err := admin.DB()
	if err != nil {
		t.Fatal("could not open fixture pool")
	}
	t.Cleanup(func() { _ = adminPool.Close() })
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "baseline_" + hex.EncodeToString(random[:])
	if err := admin.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
		t.Fatal("could not create isolated fixture schema")
	}
	t.Cleanup(func() {
		// schema is generated here from a fixed prefix and hex characters only.
		if err := admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`).Error; err != nil {
			t.Error("could not remove owned fixture schema")
		}
	})
	query.Set("search_path", schema)
	connection.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(connection.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("could not open isolated fixture")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal("could not open isolated fixture pool")
	}
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}
