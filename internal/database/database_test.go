package database

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"nordik-drive-api/config"
)

func TestDatabaseConfigurationFailsBeforeNetwork(t *testing.T) {
	for _, cfg := range []config.Config{{DBAuthMode: "auto"}, {DBAuthMode: "iam"}, {DBAuthMode: "iam", CloudSQLInstance: "project:region:instance", DBUser: "user", DBName: "db", DBPassword: "never-allowed"}, {DBAuthMode: "iam", CloudSQLInstance: "project:region:instance", DBUser: "user", DBName: "db", CloudSQLIPType: "unknown"}, {DBAuthMode: "password", DBSchema: "bad; SET role"}} {
		if _, cleanup, err := Open(context.Background(), cfg); err == nil {
			if cleanup != nil {
				cleanup()
			}
			t.Fatal("unsafe database configuration accepted")
		}
	}
}
func TestAcquisitionTimeoutAndPoolMetricsAreSeparate(t *testing.T) {
	pool, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	held, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := Acquire(context.Background(), pool, 10*time.Millisecond); err == nil {
		t.Fatal("unbounded acquisition")
	}
	stats := PoolStats(pool)
	if stats["in_use"] != 1 || stats["wait_count_total"].(int64) < 1 {
		t.Fatal("missing aggregate pool wait/saturation metrics")
	}
}
