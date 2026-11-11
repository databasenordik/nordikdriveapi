package main

import (
	"context"
	"os"
	"testing"

	"nordik-drive-api/config"
)

func TestCleanupStartupHasNoJWTOrProviderDependency(t *testing.T) {
	// Deliberately omit all application secrets and provider settings. An
	// incomplete database config must reach the maintenance database boundary.
	cfg := config.Config{DBAuthMode: "iam", DBSchema: "public", DBMaxOpen: 1}
	if err := runSignupCleanup(context.Background(), cfg); err == nil || err.Error() != "signup cleanup database initialization failed" {
		t.Fatal("maintenance constructed application dependencies", err)
	}
	for _, cfg := range []config.Config{
		{DBAuthMode: "password", DBSchema: "public", DBMaxOpen: 1},
		{DBAuthMode: "iam", DBSchema: "public", DBMaxOpen: 2},
		{DBAuthMode: "iam", DBSchema: "gateway", DBMaxOpen: 1},
	} {
		if err := runSignupCleanup(context.Background(), cfg); err == nil {
			t.Fatal("unsafe maintenance configuration accepted")
		}
	}
}

func TestUnknownMaintenanceCommandCannotStartServer(t *testing.T) {
	previous := os.Args
	os.Args = []string{"server", "unexpected-maintenance"}
	t.Cleanup(func() { os.Args = previous })
	t.Setenv("OTEL_EXPORTER", "")
	t.Setenv("JWT_SECRET", "")
	if err := run(); err == nil || err.Error() != "unknown server command" {
		t.Fatal("unknown command did not fail before application startup", err)
	}
}
