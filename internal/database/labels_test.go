package database

import (
	"strings"
	"testing"
)

func TestApplicationName(t *testing.T) {
	for _, tc := range []struct{ role, revision, execution, want string }{
		{"worker", "service-00001-abc", "", "run:service-00001-abc"},
		{"identity", "", "cleanup-abc", "job:cleanup-abc"},
		{"identity", "revision", "execution", "job:execution"},
		{"worker", "", "", "local:worker"}, {"", "", "", "local:monolith"},
	} {
		got, err := ApplicationName(tc.role, tc.revision, tc.execution)
		if err != nil || got != tc.want {
			t.Fatalf("label=%q err=%v want %q", got, err, tc.want)
		}
	}
	a, err := ApplicationName("", strings.Repeat("a", 100)+"x", "")
	b, _ := ApplicationName("", strings.Repeat("a", 100)+"y", "")
	if err != nil || len(a) > 63 || a == b {
		t.Fatal("long labels collide or exceed PostgreSQL limit")
	}
	for _, bad := range []string{"bad space", "bad\nlabel", strings.Repeat("a", 129)} {
		if _, err := ApplicationName("", bad, ""); err == nil {
			t.Fatal("invalid runtime label accepted")
		}
	}
}

func TestTransitionalSearchPathsAreExplicit(t *testing.T) {
	for _, value := range []string{"", "public", "fixture_123", "identity,public", "public,identity", "audit,identity", "public,identity,audit", "audit,public,identity"} {
		if _, err := validatedSearchPath(value); err != nil {
			t.Fatalf("valid path %q: %v", value, err)
		}
	}
	for _, value := range []string{"public,anything", "identity, public", "$user,public", "public;SET ROLE owner", "\"public\"", "public,audit"} {
		if _, err := validatedSearchPath(value); err == nil {
			t.Fatalf("unsafe path %q accepted", value)
		}
	}
}
