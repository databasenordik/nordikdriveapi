package gateway

import "strings"

// Boundaries select services, not individual endpoints. Services own their
// routers, resource authorization, and 404/405 responses.
type Boundary struct{ Prefix, Service, Auth, Group string }

var Boundaries = []Boundary{
	{"/api/user", "identity", "optional", "auth"},
	{"/api/communities", "identity", "optional", "business"},
	{"/api/role", "identity", "access", "business"},
	{"/api/file", "records", "access", "business"},
	{"/api/form", "records", "access", "business"},
	{"/api/admin", "records", "access", "business"},
	{"/api/config", "records", "access", "business"},
	{"/api/lookup", "records", "optional", "business"},
	{"/api/logo-content", "records", "access", "business"},
	{"/api/support-requests", "support", "access", "business"},
	{"/api/support-schedule", "support", "access", "business"},
	{"/api/chat", "chat", "access", "chat"},
	{"/api/logs", "audit", "access", "business"},
}
var ServiceNames = []string{"identity", "records", "support", "chat", "audit"}

func Match(_ string, path string) (Boundary, bool) {
	if strings.Contains(path, "//") || strings.Contains(path, `\`) || strings.ContainsAny(path, "\x00\r\n") {
		return Boundary{}, false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return Boundary{}, false
		}
	}
	for _, boundary := range Boundaries {
		if path == boundary.Prefix || strings.HasPrefix(path, boundary.Prefix+"/") {
			return boundary, true
		}
	}
	return Boundary{}, false
}
