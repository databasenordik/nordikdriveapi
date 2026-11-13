//go:build integration

package integration

import (
	"encoding/json"
	"reflect"
	"testing"

	"nordik-drive-api/internal/admin"
	"nordik-drive-api/internal/identityclient"
)

func TestAdminOwnerSearchPreservesPostgresResults(t *testing.T) {
	db := notificationPostgres(t)
	schema := `CREATE TABLE users(id bigint PRIMARY KEY,firstname text,lastname text);
 CREATE TABLE file(id bigint PRIMARY KEY,filename text);
 CREATE TABLE file_edit_request(request_id bigint PRIMARY KEY,file_id bigint,user_id bigint,reviewed_by bigint,status text,firstname text,lastname text,community text[],uploader_community text[],consent boolean,created_at timestamptz);
 CREATE TABLE file_edit_request_details(id bigint PRIMARY KEY,request_id bigint,field_name text,old_value text,new_value text);
 INSERT INTO users VALUES(1,'First','User'),(2,'Review','Admin');
 INSERT INTO file VALUES(5,'reports.csv');
 INSERT INTO file_edit_request VALUES
 (10,5,1,2,'approved','Stored','Name',ARRAY['North'],ARRAY['South'],true,'2026-10-01T10:00:00Z'),
 (11,5,99,98,'pending','','',NULL,NULL,false,'2026-10-02T10:00:00Z'),
 (12,5,NULL,NULL,'pending','','',NULL,NULL,false,'2026-10-03T10:00:00Z');
 INSERT INTO file_edit_request_details VALUES(1,10,'title','old','new'),(2,10,'email','',''),(3,11,'title','before','after'),(4,12,'title','a','b');`
	if err := db.Exec(schema).Error; err != nil {
		t.Fatal(err)
	}
	directory := &ownerDirectory{profiles: map[int64]identityclient.Profile{1: {ID: 1, Firstname: "First", Lastname: "User"}, 2: {ID: 2, Firstname: "Review", Lastname: "Admin"}}}
	legacy := &admin.AdminService{DB: db}
	owned := &admin.AdminService{DB: db, Directory: directory}
	requests := []admin.AdminFileEditSearchRequest{{Page: 1, PageSize: 2}, {Page: 2, PageSize: 2}, {Page: 1, PageSize: 10, Clauses: []admin.Clause{{Field: "file_id", Op: admin.OpEQ, Value: func() *string { s := "5"; return &s }()}}}}
	expected := make([][]byte, len(requests))
	for i, req := range requests {
		result, err := legacy.SearchFileEditRequests(req)
		if err != nil {
			t.Fatal(err)
		}
		expected[i], err = json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrator().RenameTable("users", "identity_owned_users"); err != nil {
		t.Fatal(err)
	}
	for i, req := range requests {
		result, err := owned.SearchFileEditRequests(req)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected[i]) {
			t.Fatalf("owner search changed page/filter %d:\n%s\n%s", i, expected[i], actual)
		}
	}
	directory.fail = true
	if result, err := owned.SearchFileEditRequests(requests[0]); err == nil || result != nil {
		t.Fatal("owner outage returned partial success")
	}
}
