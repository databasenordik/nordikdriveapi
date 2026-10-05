// internal/file/service_test.go
package file

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/glebarez/sqlite"
	"github.com/xuri/excelize/v2"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"nordik-drive-api/internal/dataconfig"
	"nordik-drive-api/internal/honour"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/util"
)

// -----------------------------------------------------------------------------
// Test DB helpers (sqlite in-memory, isolated per test)
// -----------------------------------------------------------------------------

var testDBSeq uint64

// --- Add this near the top of service_test.go ---

// UserForTest creates the exact "users" table your FileService queries/join against.
type UserForTest struct {
	ID        uint   `gorm:"primaryKey;column:id"`
	Email     string `gorm:"column:email"`
	Role      string `gorm:"column:role"`
	FirstName string `gorm:"column:firstname"`
	LastName  string `gorm:"column:lastname"`
}

func (UserForTest) TableName() string { return "users" }

type FormSubmissionForTest struct {
	ID       int64  `gorm:"primaryKey;column:id"`
	FileID   uint   `gorm:"column:file_id"`
	RowID    int64  `gorm:"column:row_id"`
	FileName string `gorm:"column:file_name"`
	Status   string `gorm:"column:status"`
}

func (FormSubmissionForTest) TableName() string { return "form_submissions" }

func migrateTestSchema(t *testing.T, db *gorm.DB) {
	t.Helper()

	// optional but nice for sqlite tests
	_ = db.Exec("PRAGMA foreign_keys = ON").Error

	// IMPORTANT: migrate ALL tables your service touches
	if err := db.AutoMigrate(
		&File{},
		&FileVersion{},
		&FileData{},
		&FileDataNormalized{},
		&FileVersionReconciliationJob{},
		&FileRowLineage{},
		&honour.DailyHonour{},
		&FormSubmissionForTest{},
		&FileAccess{},
		&dataconfig.DataConfig{},
		&FileEditRequest{},
		&FileEditRequestDetails{},
		&FileEditRequestPhoto{},
		&AchieverStory{},
		&UserForTest{}, // creates "users"
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	id := atomic.AddUint64(&testDBSeq, 1)
	dsn := fmt.Sprintf("file:nordik_test_%d?mode=memory&cache=shared", id)

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

	// ✅ CREATE TABLES HERE
	migrateTestSchema(t, db)

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

// -----------------------------------------------------------------------------
// Multipart helpers
// -----------------------------------------------------------------------------

func fileHeaderFromBytes(t *testing.T, formField, filename string, content []byte) *multipart.FileHeader {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(formField, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = fw.Write(content)
	_ = w.Close()

	r := multipart.NewReader(bytes.NewReader(buf.Bytes()), w.Boundary())
	form, err := r.ReadForm(32 << 20)
	if err != nil {
		t.Fatalf("ReadForm: %v", err)
	}

	fhs := form.File[formField]
	if len(fhs) != 1 {
		t.Fatalf("expected 1 file header, got %d", len(fhs))
	}
	return fhs[0]
}

func csvBytes(headers []string, rows [][]string) []byte {
	var b strings.Builder
	b.WriteString(strings.Join(headers, ","))
	b.WriteString("\n")
	for _, r := range rows {
		b.WriteString(strings.Join(r, ","))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func xlsxBytesWithStyles(t *testing.T) []byte {
	t.Helper()

	f := excelize.NewFile()
	sheet := f.GetSheetName(0)

	// headers
	_ = f.SetCellValue(sheet, "A1", "Name")
	_ = f.SetCellValue(sheet, "B1", "Age")

	// data
	_ = f.SetCellValue(sheet, "A2", "John")
	_ = f.SetCellValue(sheet, "B2", "30")

	// style with fill color that maps in parseExcelReader:
	// "#FFFF99" => "FURTHER INVESTIGATION REQUIRED"
	styleID, err := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{
			Type:    "pattern",
			Pattern: 1,
			Color:   []string{"FFFF99"},
		},
	})
	if err != nil {
		t.Fatalf("NewStyle: %v", err)
	}

	// Apply style to A2 (has value => should append)
	if err := f.SetCellStyle(sheet, "A2", "A2", styleID); err != nil {
		t.Fatalf("SetCellStyle: %v", err)
	}

	// Apply style to B2 but set empty value (no append)
	_ = f.SetCellValue(sheet, "B2", "")
	if err := f.SetCellStyle(sheet, "B2", "B2", styleID); err != nil {
		t.Fatalf("SetCellStyle: %v", err)
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("WriteToBuffer: %v", err)
	}
	return buf.Bytes()
}

func xlsxBytesWithShortDateCell(t *testing.T) []byte {
	t.Helper()

	f := excelize.NewFile()
	sheet := f.GetSheetName(0)

	_ = f.SetCellValue(sheet, "A1", "BuiltInDate")
	_ = f.SetCellValue(sheet, "B1", "TextDate")

	// Excel serial 988 formatted with built-in short date (numFmt 14) is the
	// same case that previously imported as "09-14-02".
	_ = f.SetCellValue(sheet, "A2", 988)
	_ = f.SetCellValue(sheet, "B2", "1898-08-06")

	styleID, err := f.NewStyle(&excelize.Style{NumFmt: 14})
	if err != nil {
		t.Fatalf("NewStyle: %v", err)
	}
	if err := f.SetCellStyle(sheet, "A2", "A2", styleID); err != nil {
		t.Fatalf("SetCellStyle: %v", err)
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("WriteToBuffer: %v", err)
	}
	return buf.Bytes()
}

// -----------------------------------------------------------------------------
// Fake GCS server helper (covers GetPhotoBytes/GetDocBytes/OpenMediaHandle/readFromGCS)
// -----------------------------------------------------------------------------

func withFakeGCS(t *testing.T) (*fakestorage.Server, string) {
	t.Helper()

	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
	})
	if err != nil {
		t.Fatalf("failed to start fake gcs: %v", err)
	}
	t.Cleanup(srv.Stop)

	bucket := "test-bucket"
	srv.CreateBucket(bucket)

	_ = os.Setenv("BUCKET_NAME", bucket)

	// ✅ CRITICAL: use srv.Client() so NewReader hits supported endpoints
	prev := newGCSClientHook
	newGCSClientHook = func(ctx context.Context) (*storage.Client, error) {
		return srv.Client(), nil
	}
	t.Cleanup(func() { newGCSClientHook = prev })

	return srv, bucket
}

// setFieldIfExists sets a struct field by name if it exists and is settable.
// It also tolerates slightly different fake-gcs-server versions (field names changed across versions).
func setFieldIfExists(rv reflect.Value, field string, val any) {
	f := rv.FieldByName(field)
	if !f.IsValid() || !f.CanSet() {
		return
	}

	v := reflect.ValueOf(val)

	// Handle []byte assignment into []uint8
	if f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.Uint8 && v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
		f.SetBytes(v.Bytes())
		return
	}

	// Direct assign if types match or convertible
	if v.Type().AssignableTo(f.Type()) {
		f.Set(v)
		return
	}
	if v.Type().ConvertibleTo(f.Type()) {
		f.Set(v.Convert(f.Type()))
		return
	}
}

func putGCSObject(t *testing.T, srv *fakestorage.Server, bucket, name string, content []byte, contentType string) {
	t.Helper()
	_ = srv // keep signature

	ctx := context.Background()

	// ✅ IMPORTANT: use the same hook the service uses
	client, err := newGCSClientHook(ctx)
	if err != nil {
		t.Fatalf("newGCSClientHook: %v", err)
	}
	defer client.Close()

	w := client.Bucket(bucket).Object(name).NewWriter(ctx)
	if contentType != "" {
		w.ContentType = contentType
	}

	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		t.Fatalf("write object %s/%s: %v", bucket, name, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer %s/%s: %v", bucket, name, err)
	}
}

// -----------------------------------------------------------------------------
// Unsafe helper to force *storage.Reader internal reader so we can fully cover ioReadAll
// storage.Reader has unexported fields; we set them via reflect+unsafe.
// -----------------------------------------------------------------------------

type errReadCloser struct {
	readFn  func([]byte) (int, error)
	closeFn func() error
}

func (e *errReadCloser) Read(p []byte) (int, error) { return e.readFn(p) }
func (e *errReadCloser) Close() error {
	if e.closeFn != nil {
		return e.closeFn()
	}
	return nil
}

func forceStorageReaderInner(t *testing.T, sr *storage.Reader, inner io.ReadCloser) {
	t.Helper()

	rv := reflect.ValueOf(sr).Elem()

	// Set ctx to a valid context to avoid nil issues in some versions
	if f := rv.FieldByName("ctx"); f.IsValid() {
		setUnexported(t, f, reflect.ValueOf(context.Background()))
	}

	// Prefer field named "reader"
	if f := rv.FieldByName("reader"); f.IsValid() {
		setUnexported(t, f, reflect.ValueOf(inner))
		return
	}

	// Fallback: set first io.ReadCloser field
	rcType := reflect.TypeOf((*io.ReadCloser)(nil)).Elem()
	for i := 0; i < rv.NumField(); i++ {
		ft := rv.Type().Field(i).Type
		if ft == rcType {
			setUnexported(t, rv.Field(i), reflect.ValueOf(inner))
			return
		}
	}

	t.Fatalf("could not find storage.Reader inner reader field")
}

func setUnexported(t *testing.T, field reflect.Value, val reflect.Value) {
	t.Helper()
	if !field.IsValid() {
		t.Fatalf("invalid field")
	}
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(val)
}

// -----------------------------------------------------------------------------
// normalizeColorHex / parseStatuses / parseGSURL tests
// -----------------------------------------------------------------------------

func TestNormalizeColorHex_AllBranches(t *testing.T) {
	if got := normalizeColorHex(""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := normalizeColorHex("0xFFffff99"); got != "#FFFF99" {
		t.Fatalf("ARGB drop alpha expected #FFFF99, got %q", got)
	}
	if got := normalizeColorHex("#abc"); got != "#AABBCC" {
		t.Fatalf("3-digit expected #AABBCC, got %q", got)
	}
	if got := normalizeColorHex("12"); got != "#120000" {
		t.Fatalf("pad expected #120000, got %q", got)
	}
	if got := normalizeColorHex("A1B2C3D4E5"); got != "#A1B2C3" {
		t.Fatalf("trim expected #A1B2C3, got %q", got)
	}
	if got := normalizeColorHex("ffffff"); got != "#FFFFFF" {
		t.Fatalf("6-digit expected #FFFFFF, got %q", got)
	}
}

func TestExcelImportShortDatePattern_DefaultAndEnvOverride(t *testing.T) {
	t.Setenv(excelShortDatePatternEnv, "")
	if got := excelImportShortDatePattern(); got != defaultExcelImportShortDatePattern {
		t.Fatalf("default pattern = %q, want %q", got, defaultExcelImportShortDatePattern)
	}

	t.Setenv(excelShortDatePatternEnv, "yyyy/mm/dd")
	if got := excelImportShortDatePattern(); got != "yyyy/mm/dd" {
		t.Fatalf("env override pattern = %q, want %q", got, "yyyy/mm/dd")
	}
}

func TestParseStatuses_AllBranches(t *testing.T) {
	if got, err := parseStatuses("   "); err != nil || got != nil {
		t.Fatalf("expected nil, got %#v", got)
	}
	got, err := parseStatuses(" Pending, completed, pending,  ,COMPLETED ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "pending" || got[1] != "completed" {
		t.Fatalf("unexpected: %#v", got)
	}
	if got, err := parseStatuses(",,,   ,"); err != nil || got != nil {
		t.Fatalf("expected nil for garbage, got %#v", got)
	}
	if _, err := parseStatuses("approved"); err == nil || err.Error() != "status must be pending or completed" {
		t.Fatalf("expected invalid parent status error, got %v", err)
	}
}

func TestParseGSURL_AllBranches(t *testing.T) {
	if _, _, err := parseGSURL(""); err == nil {
		t.Fatalf("expected error")
	}
	if _, _, err := parseGSURL("http://x"); err == nil {
		t.Fatalf("expected error")
	}
	if _, _, err := parseGSURL("gs://bucketonly"); err == nil {
		t.Fatalf("expected error")
	}
	if _, _, err := parseGSURL("gs://bucket/"); err == nil {
		t.Fatalf("expected error")
	}

	// bucket may be empty but object path must exist (covers bucket-from-env branch in OpenMediaHandle/readFromGCS)
	b, obj, err := parseGSURL("gs:///obj.txt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if b != "" || obj != "obj.txt" {
		t.Fatalf("unexpected parsed: bucket=%q obj=%q", b, obj)
	}

	b, obj, err = parseGSURL("gs://myb/folder/a.txt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if b != "myb" || obj != "folder/a.txt" {
		t.Fatalf("unexpected parsed: bucket=%q obj=%q", b, obj)
	}
}

// -----------------------------------------------------------------------------
// parseCSVReader / parseExcelReader
// -----------------------------------------------------------------------------

func TestParseCSVReader_EmptyAndInvalidAndOK(t *testing.T) {
	// empty => error
	fh := fileHeaderFromBytes(t, "file", "a.csv", []byte(""))
	f, _ := fh.Open()
	defer f.Close()
	if _, _, err := parseCSVReader(f); err == nil {
		t.Fatalf("expected error for empty csv")
	}

	// invalid => error
	bad := []byte(`a,b` + "\n" + `"unterminated`)
	fh = fileHeaderFromBytes(t, "file", "b.csv", bad)
	f, _ = fh.Open()
	defer f.Close()
	if _, _, err := parseCSVReader(f); err == nil {
		t.Fatalf("expected error for invalid csv")
	}

	// ok
	ok := csvBytes([]string{"h1", "h2"}, [][]string{{"v1", "v2"}})
	fh = fileHeaderFromBytes(t, "file", "c.csv", ok)
	f, _ = fh.Open()
	defer f.Close()
	h, rows, err := parseCSVReader(f)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(h) != 2 || h[0] != "h1" || len(rows) != 1 || rows[0][0] != "v1" {
		t.Fatalf("unexpected parse: headers=%v rows=%v", h, rows)
	}
}

func TestParseExcelReader_InvalidAndOK_WithColorAppend(t *testing.T) {
	// invalid bytes => parse error
	fh := fileHeaderFromBytes(t, "file", "x.xlsx", []byte("notxlsx"))
	f, _ := fh.Open()
	defer f.Close()
	if _, _, err := parseExcelReader(f); err == nil {
		t.Fatalf("expected error")
	}

	// ok with style mapping
	xbytes := xlsxBytesWithStyles(t)
	fh = fileHeaderFromBytes(t, "file", "ok.xlsx", xbytes)
	f, _ = fh.Open()
	defer f.Close()

	headers, data, err := parseExcelReader(f)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(headers) != 2 || headers[0] != "Name" || headers[1] != "Age" {
		t.Fatalf("unexpected headers: %#v", headers)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 data row, got %d", len(data))
	}
	if !strings.Contains(data[0][0], "FURTHER INVESTIGATION REQUIRED") {
		t.Fatalf("expected appended source, got %q", data[0][0])
	}
	// B2 was empty, so no append
	if data[0][1] != "" {
		t.Fatalf("expected empty age, got %q", data[0][1])
	}
}

func TestParseExcelReader_NormalizesBuiltInShortDates_AndKeepsTextDates(t *testing.T) {
	fh := fileHeaderFromBytes(t, "file", "dates.xlsx", xlsxBytesWithShortDateCell(t))
	f, _ := fh.Open()
	defer f.Close()

	headers, data, err := parseExcelReader(f)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(headers) != 2 || headers[0] != "BuiltInDate" || headers[1] != "TextDate" {
		t.Fatalf("unexpected headers: %#v", headers)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 data row, got %d", len(data))
	}
	if data[0][0] != "1902-09-14" {
		t.Fatalf("expected normalized short date, got %q", data[0][0])
	}
	if data[0][1] != "1898-08-06" {
		t.Fatalf("expected text date to stay as-is, got %q", data[0][1])
	}
}

// -----------------------------------------------------------------------------
// SaveFilesMultipart
// -----------------------------------------------------------------------------

func TestFileService_SaveFilesMultipart_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	// duplicate filename
	if err := db.Create(&File{Filename: "dup"}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	fh := fileHeaderFromBytes(t, "file", "dup.csv", csvBytes([]string{"a"}, [][]string{{"1"}}))
	_, err := svc.SaveFilesMultipart([]*multipart.FileHeader{fh}, FileUploadInput{
		FileNames:       []string{"dup"},
		Private:         []bool{false},
		CommunityFilter: []bool{true},
	}, 1)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate error, got %v", err)
	}

	// unsupported extension
	fh = fileHeaderFromBytes(t, "file", "x.txt", []byte("hi"))
	_, err = svc.SaveFilesMultipart([]*multipart.FileHeader{fh}, FileUploadInput{
		FileNames:       []string{"x"},
		Private:         []bool{false},
		CommunityFilter: []bool{true},
	}, 1)
	if err == nil || !strings.Contains(err.Error(), "unsupported file type") {
		t.Fatalf("expected unsupported error, got %v", err)
	}

	// CSV parse error
	fh = fileHeaderFromBytes(t, "file", "bad.csv", []byte(`h1`+"\n"+`"unterminated`))
	_, err = svc.SaveFilesMultipart([]*multipart.FileHeader{fh}, FileUploadInput{
		FileNames:       []string{"bad"},
		Private:         []bool{false},
		CommunityFilter: []bool{true},
	}, 1)
	if err == nil {
		t.Fatalf("expected csv parse err")
	}

	// Excel parse error
	fh = fileHeaderFromBytes(t, "file", "bad.xlsx", []byte("notxlsx"))
	_, err = svc.SaveFilesMultipart([]*multipart.FileHeader{fh}, FileUploadInput{
		FileNames:       []string{"badx"},
		Private:         []bool{false},
		CommunityFilter: []bool{true},
	}, 1)
	if err == nil {
		t.Fatalf("expected excel parse err")
	}

	// success CSV (also covers: missing cells => empty string)
	fh = fileHeaderFromBytes(t, "file", "ok.csv", csvBytes(
		[]string{"c1", "c2", "c3"},
		[][]string{
			{"a", "b", ""},
			{"x", "y", "z"},
		},
	))

	out, err := svc.SaveFilesMultipart([]*multipart.FileHeader{fh}, FileUploadInput{
		FileNames:       []string{"okfile"},
		Private:         []bool{true},
		CommunityFilter: []bool{true},
		Descriptions:    []string{"School register and death details for reconciliation testing."},
	}, 99)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 1 || out[0].Filename != "okfile" || out[0].Version != 1 || out[0].Rows != 2 || out[0].InsertedBy != 99 {
		t.Fatalf("unexpected file out: %#v", out)
	}
	if out[0].Description != "School register and death details for reconciliation testing." {
		t.Fatalf("expected description to be stored on file, got %#v", out[0])
	}

	// verify file_version created
	var vers []FileVersion
	if err := db.Where("file_id = ?", out[0].ID).Find(&vers).Error; err != nil {
		t.Fatalf("fetch versions: %v", err)
	}
	if len(vers) != 1 || vers[0].Version != 1 {
		t.Fatalf("unexpected versions: %#v", vers)
	}
	if vers[0].Description != "School register and death details for reconciliation testing." {
		t.Fatalf("expected file version description to be stored, got %#v", vers[0])
	}
	if string(vers[0].ColumnsOrder) != `["c1","c2","c3"]` {
		t.Fatalf("expected file version columns order to be stored, got %s", string(vers[0].ColumnsOrder))
	}

	// verify file_data created and has c3 filled for first row
	var rows []FileData
	if err := db.Where("file_id = ?", out[0].ID).Order("id asc").Find(&rows).Error; err != nil {
		t.Fatalf("fetch data: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	var m map[string]string
	_ = json.Unmarshal(rows[0].RowData, &m)
	if m["c3"] != "" || m["c1"] != "a" || m["c2"] != "b" {
		t.Fatalf("unexpected row map: %#v", m)
	}
}

func TestFileService_SaveFilesMultipart_StoresExcelShortDatesAsISO(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	fh := fileHeaderFromBytes(t, "file", "dates.xlsx", xlsxBytesWithShortDateCell(t))
	out, err := svc.SaveFilesMultipart([]*multipart.FileHeader{fh}, FileUploadInput{
		FileNames:       []string{"dates-file"},
		Private:         []bool{false},
		CommunityFilter: []bool{true},
	}, 7)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 saved file, got %d", len(out))
	}

	var rows []FileData
	if err := db.Where("file_id = ?", out[0].ID).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("fetch data: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 stored row, got %d", len(rows))
	}

	var row map[string]string
	if err := json.Unmarshal(rows[0].RowData, &row); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	if row["BuiltInDate"] != "1902-09-14" {
		t.Fatalf("expected uploaded date to be stored as ISO, got %q", row["BuiltInDate"])
	}
	if row["TextDate"] != "1898-08-06" {
		t.Fatalf("expected text date to stay as-is, got %q", row["TextDate"])
	}
}

// -----------------------------------------------------------------------------
// ReplaceFiles
// -----------------------------------------------------------------------------

func TestFileService_ReplaceFiles_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	// not found
	fh := fileHeaderFromBytes(t, "file", "x.csv", csvBytes([]string{"a"}, [][]string{{"1"}}))
	if err := svc.ReplaceFiles(fh, 999999, 1); err == nil {
		t.Fatalf("expected not found")
	}

	// seed file
	f := File{Filename: "f1", Version: 1, Private: false, IsDelete: false}
	if err := db.Create(&f).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// unsupported extension
	fh = fileHeaderFromBytes(t, "file", "x.txt", []byte("hi"))
	if err := svc.ReplaceFiles(fh, f.ID, 1); err == nil {
		t.Fatalf("expected unsupported")
	}

	// parse error
	fh = fileHeaderFromBytes(t, "file", "bad.csv", []byte(`h1`+"\n"+`"unterminated`))
	if err := svc.ReplaceFiles(fh, f.ID, 1); err == nil {
		t.Fatalf("expected parse error")
	}

	// blocked while another transition is already queued
	if err := db.Create(&FileVersion{
		FileID:               f.ID,
		Filename:             f.Filename,
		Version:              2,
		ReconciliationStatus: fileVersionStatusProcessing,
	}).Error; err != nil {
		t.Fatalf("seed queued version: %v", err)
	}
	fh = fileHeaderFromBytes(t, "file", "ok.csv", csvBytes([]string{"h1"}, [][]string{{"v1"}}))
	if err := svc.ReplaceFiles(fh, f.ID, 1); !errors.Is(err, ErrFileVersionTransitionInProgress) {
		t.Fatalf("expected transition in progress err, got %v", err)
	}

	// force Save error by dropping table
	fh = fileHeaderFromBytes(t, "file", "ok.csv", csvBytes([]string{"h1"}, [][]string{{"v1"}}))
	if err := db.Migrator().DropTable(&File{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if err := svc.ReplaceFiles(fh, f.ID, 1); err == nil {
		t.Fatalf("expected save error")
	}

	// re-migrate and re-seed for success + other create errors
	db = newTestDB(t)
	svc = &FileService{DB: db}
	f = File{Filename: "f1", Version: 1, Private: false, IsDelete: false}
	_ = db.Create(&f).Error

	// Create file_version error
	fh = fileHeaderFromBytes(t, "file", "ok.csv", csvBytes([]string{"h1"}, [][]string{{"v1"}}))
	if err := db.Migrator().DropTable(&FileVersion{}); err != nil {
		t.Fatalf("drop file_version: %v", err)
	}
	if err := svc.ReplaceFiles(fh, f.ID, 2); err == nil {
		t.Fatalf("expected file_version insert error")
	}

	// re-migrate and test FileData insert error
	db = newTestDB(t)
	svc = &FileService{DB: db}
	f = File{Filename: "f1", Version: 1, Private: false, IsDelete: false}
	_ = db.Create(&f).Error

	if err := db.Migrator().DropTable(&FileData{}); err != nil {
		t.Fatalf("drop file_data: %v", err)
	}
	fh = fileHeaderFromBytes(t, "file", "ok.csv", csvBytes([]string{"h1"}, [][]string{{"v1"}}))
	if err := svc.ReplaceFiles(fh, f.ID, 2); err == nil {
		t.Fatalf("expected file_data insert error")
	}

	// success
	db = newTestDB(t)
	svc = &FileService{DB: db}
	f = File{
		Filename:     "f1",
		Version:      1,
		Private:      true,
		IsDelete:     false,
		ColumnsOrder: datatypes.JSON([]byte(`["old_col"]`)),
	}
	_ = db.Create(&f).Error

	fh = fileHeaderFromBytes(t, "file", "ok.csv", csvBytes([]string{"h1", "h2"}, [][]string{{"v1", ""}}))
	if err := svc.ReplaceFiles(fh, f.ID, 77); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var updated File
	if err := db.First(&updated, f.ID).Error; err != nil {
		t.Fatalf("fetch updated: %v", err)
	}
	if updated.Version != 1 || updated.Rows != 0 {
		t.Fatalf("unexpected queued file state: %#v", updated)
	}
	if string(updated.ColumnsOrder) != `["old_col"]` {
		t.Fatalf("expected current file columns order unchanged before reconciliation, got %s", string(updated.ColumnsOrder))
	}

	var queuedJobs int64
	if err := db.Model(&FileVersionReconciliationJob{}).Where("file_id = ?", f.ID).Count(&queuedJobs).Error; err != nil {
		t.Fatalf("count queued jobs: %v", err)
	}
	if queuedJobs != 1 {
		t.Fatalf("expected 1 queued reconciliation job, got %d", queuedJobs)
	}

	var v []FileVersion
	_ = db.Where("file_id = ?", f.ID).Find(&v).Error
	if len(v) != 1 || v[0].Version != 2 || v[0].InsertedBy != 77 || v[0].ReconciliationStatus != fileVersionStatusProcessing {
		t.Fatalf("unexpected queued version rows: %#v", v)
	}
	if string(v[0].ColumnsOrder) != `["h1","h2"]` {
		t.Fatalf("expected queued version columns order, got %s", string(v[0].ColumnsOrder))
	}

	if _, err := RunVersionReconciliationJobs(db, VersionReconciliationRunOptions{MaxJobs: 1}); err != nil {
		t.Fatalf("run reconciliation jobs: %v", err)
	}

	if err := db.First(&updated, f.ID).Error; err != nil {
		t.Fatalf("fetch reconciled file: %v", err)
	}
	if updated.Version != 2 || updated.Rows != 1 {
		t.Fatalf("unexpected updated file: %#v", updated)
	}
	if string(updated.ColumnsOrder) != `["h1","h2"]` {
		t.Fatalf("expected updated file columns order, got %s", string(updated.ColumnsOrder))
	}

	_ = db.Where("file_id = ?", f.ID).Find(&v).Error
	if len(v) != 1 || v[0].Version != 2 || v[0].InsertedBy != 77 || v[0].ReconciliationStatus != fileVersionStatusReady {
		t.Fatalf("unexpected reconciled version rows: %#v", v)
	}
	if string(v[0].ColumnsOrder) != `["h1","h2"]` {
		t.Fatalf("expected version columns order, got %s", string(v[0].ColumnsOrder))
	}
}

func TestReplaceFiles_ClearsDailyHonours(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}
	file := File{Filename: "f1", Version: 1, Private: true, IsDelete: false}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("create file: %v", err)
	}
	row := FileData{FileID: file.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"name":"alpha"}`))}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("create row: %v", err)
	}
	if err := db.Create(&honour.DailyHonour{
		FileID:      file.ID,
		FileVersion: 1,
		SourceRowID: row.ID,
		HonourDate:  time.Date(2026, time.July, 14, 0, 0, 0, 0, time.UTC),
		CycleNumber: 1,
		HonourText:  "Remembered today",
		Status:      "ready",
	}).Error; err != nil {
		t.Fatalf("create honour: %v", err)
	}

	fh := fileHeaderFromBytes(t, "file", "ok.csv", csvBytes([]string{"h1"}, [][]string{{"v1"}}))
	if err := svc.ReplaceFiles(fh, file.ID, 77); err != nil {
		t.Fatalf("replace file: %v", err)
	}

	var count int64
	if err := db.Model(&honour.DailyHonour{}).Where("file_id = ?", file.ID).Count(&count).Error; err != nil {
		t.Fatalf("count honours: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected honours to be cleared, got %d", count)
	}
}

// -----------------------------------------------------------------------------
// GetUserRole / GetAllFiles
// -----------------------------------------------------------------------------

func TestFileService_GetUserRole_OK_NotFound_DBBroken(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	u := UserForTest{Email: "a@b.com", Role: "Admin", FirstName: "A", LastName: "B"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	role, err := svc.GetUserRole(uint(u.ID))
	if err != nil || role != "Admin" {
		t.Fatalf("expected Admin, got %q err=%v", role, err)
	}

	_, err = svc.GetUserRole(999999)
	if err == nil {
		t.Fatalf("expected not found err")
	}

	breakDB(t, db)
	_, err = svc.GetUserRole(uint(u.ID))
	if err == nil {
		t.Fatalf("expected db error")
	}
}

func TestFileService_GetAllFiles_AdminAndUserAndDBError(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	u1 := UserForTest{Email: "u1@b.com", Role: "User", FirstName: "U1", LastName: "L1"}
	u2 := UserForTest{Email: "u2@b.com", Role: "User", FirstName: "U2", LastName: "L2"}

	_ = db.Create(&u1).Error
	_ = db.Create(&u2).Error

	// public file by u1
	fPub := File{Filename: "pub", InsertedBy: uint(u1.ID), Private: false, IsDelete: false}
	// private file by u1
	fPriv := File{Filename: "priv", InsertedBy: uint(u1.ID), Private: true, IsDelete: false}
	// deleted private with access should NOT show for user
	fPrivDel := File{Filename: "privdel", InsertedBy: uint(u1.ID), Private: true, IsDelete: true}
	_ = db.Create(&fPub).Error
	_ = db.Create(&fPriv).Error
	_ = db.Create(&fPrivDel).Error

	// grant access for u2 to priv + privdel
	_ = db.Create(&FileAccess{UserID: uint(u2.ID), FileID: fPriv.ID}).Error
	_ = db.Create(&FileAccess{UserID: uint(u2.ID), FileID: fPrivDel.ID}).Error

	// Admin -> all (3 files)
	all, err := svc.GetAllFiles(uint(u2.ID), "Admin")
	if err != nil || len(all) != 3 {
		t.Fatalf("admin expected 3, got %d err=%v %#v", len(all), err, all)
	}

	// User -> public + private with access AND not deleted
	userFiles, err := svc.GetAllFiles(uint(u2.ID), "User")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// should include pub + priv (NOT privdel)
	if len(userFiles) != 2 {
		t.Fatalf("expected 2, got %d: %#v", len(userFiles), userFiles)
	}

	breakDB(t, db)
	_, err = svc.GetAllFiles(uint(u2.ID), "Admin")
	if err == nil {
		t.Fatalf("expected db err")
	}
}

// -----------------------------------------------------------------------------
// GetFileData
// -----------------------------------------------------------------------------

func TestFileService_GetFileData_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	// not found => nil,nil
	got, err := svc.GetFileData("missing", 1)
	if err != nil || got != nil {
		t.Fatalf("expected nil,nil got=%v err=%v", got, err)
	}

	// create file with invalid ColumnsOrder => unmarshal error
	f := File{Filename: "f1", IsDelete: false, ColumnsOrder: []byte(`not-json`)}
	_ = db.Create(&f).Error
	_, err = svc.GetFileData("f1", 1)
	if err == nil || !strings.Contains(err.Error(), "unmarshal columns order") {
		t.Fatalf("expected columns order error, got %v", err)
	}

	// valid columns order + invalid row json => row unmarshal error
	cols, _ := json.Marshal([]string{"b", "a", "c"})
	f2 := File{Filename: "f2", IsDelete: false, ColumnsOrder: cols}
	_ = db.Create(&f2).Error
	_ = db.Create(&FileData{
		FileID:  f2.ID,
		RowData: datatypes.JSON([]byte(`{bad json`)),
		Version: 1,
	}).Error
	_, err = svc.GetFileData("f2", 1)
	if err == nil || !strings.Contains(err.Error(), "unmarshal row") {
		t.Fatalf("expected row unmarshal error, got %v", err)
	}

	// success reorder + fill missing columns using the requested version schema
	currentCols, _ := json.Marshal([]string{"current_only"})
	f3 := File{Filename: "f3", IsDelete: false, Version: 6, ColumnsOrder: currentCols}
	_ = db.Create(&f3).Error
	_ = db.Create(&FileVersion{
		FileID:       f3.ID,
		Filename:     "f3",
		Version:      5,
		ColumnsOrder: cols,
	}).Error
	// Row has keys out of order, missing "c", and fields absent from saved column order.
	_ = db.Create(&FileData{
		FileID:  f3.ID,
		RowData: datatypes.JSON([]byte(`{"a":"1","b":"2","Name Comments":"Stored annotation","New Field":"Preserved"}`)),
		Version: 5,
	}).Error

	rows, err := svc.GetFileData("f3", 5)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, rows[0].RowData); err != nil {
		t.Fatalf("compact ordered row: %v", err)
	}
	if compact.String() != `{"b":"2","a":"1","c":"","Name Comments":"Stored annotation","New Field":"Preserved"}` {
		t.Fatalf("unexpected ordered row: %s", compact.String())
	}

	// duplicate file_version rows from legacy data should not break reads
	f4 := File{Filename: "f4", IsDelete: false, Version: 1, ColumnsOrder: cols}
	_ = db.Create(&f4).Error
	_ = db.Create(&FileVersion{
		FileID:               f4.ID,
		Filename:             "f4",
		Version:              1,
		ColumnsOrder:         cols,
		ReconciliationStatus: fileVersionStatusReady,
	}).Error
	_ = db.Create(&FileVersion{
		FileID:               f4.ID,
		Filename:             "f4",
		Version:              1,
		ColumnsOrder:         cols,
		ReconciliationStatus: fileVersionStatusReady,
	}).Error
	_ = db.Create(&FileData{
		FileID:  f4.ID,
		RowData: datatypes.JSON([]byte(`{"a":"x","b":"y"}`)),
		Version: 1,
	}).Error

	rows, err = svc.GetFileData("f4", 1)
	if err != nil {
		t.Fatalf("expected duplicate version metadata not to break reads, got %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for duplicate-version file, got %d", len(rows))
	}

	// duplicate version metadata with a newer processing row should still prefer
	// the latest ready snapshot for historical reads.
	f5 := File{Filename: "f5", IsDelete: false, Version: 2, ColumnsOrder: cols}
	_ = db.Create(&f5).Error
	_ = db.Create(&FileVersion{
		FileID:               f5.ID,
		Filename:             "f5",
		Version:              1,
		ColumnsOrder:         cols,
		ReconciliationStatus: fileVersionStatusReady,
		CreatedAt:            time.Now().Add(-time.Minute),
	}).Error
	_ = db.Create(&FileVersion{
		FileID:               f5.ID,
		Filename:             "f5",
		Version:              1,
		ColumnsOrder:         datatypes.JSON([]byte(`["a"]`)),
		ReconciliationStatus: fileVersionStatusProcessing,
		CreatedAt:            time.Now(),
	}).Error
	_ = db.Create(&FileData{
		FileID:  f5.ID,
		RowData: datatypes.JSON([]byte(`{"a":"1","b":"2"}`)),
		Version: 1,
	}).Error

	rows, err = svc.GetFileData("f5", 1)
	if err != nil {
		t.Fatalf("expected historical read to prefer ready duplicate version, got %v", err)
	}
	var readyPreferred bytes.Buffer
	if err := json.Compact(&readyPreferred, rows[0].RowData); err != nil {
		t.Fatalf("compact ready-preferred row: %v", err)
	}
	if readyPreferred.String() != `{"b":"2","a":"1","c":""}` {
		t.Fatalf("unexpected ready-preferred row order: %s", readyPreferred.String())
	}

	// Find error path: drop file_data table after file exists + columns ok
	if err := db.Migrator().DropTable(&FileData{}); err != nil {
		t.Fatalf("drop file_data: %v", err)
	}
	_, err = svc.GetFileData("f3", 5)
	if err == nil {
		t.Fatalf("expected find error")
	}
}

// -----------------------------------------------------------------------------
// DeleteFile / ResetFile
// -----------------------------------------------------------------------------

func TestFileService_DeleteAndReset_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	// not found
	if _, err := svc.DeleteFile("999"); err == nil {
		t.Fatalf("expected err")
	}
	if _, err := svc.ResetFile("999"); err == nil {
		t.Fatalf("expected err")
	}

	f := File{Filename: "f1", IsDelete: false}
	_ = db.Create(&f).Error

	// update error by dropping table
	_ = db.Migrator().DropTable(&File{})
	if _, err := svc.DeleteFile(fmt.Sprintf("%d", f.ID)); err == nil {
		t.Fatalf("expected update err")
	}

	// success delete/reset
	db = newTestDB(t)
	svc = &FileService{DB: db}
	f = File{Filename: "f2", IsDelete: false}
	_ = db.Create(&f).Error

	_, err := svc.DeleteFile(fmt.Sprintf("%d", f.ID))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	var ff File
	_ = db.First(&ff, f.ID).Error
	if ff.IsDelete != true {
		t.Fatalf("expected deleted=true")
	}

	_, err = svc.ResetFile(fmt.Sprintf("%d", f.ID))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	_ = db.First(&ff, f.ID).Error
	if ff.IsDelete != false {
		t.Fatalf("expected deleted=false")
	}
}

// -----------------------------------------------------------------------------
// Access CRUD + history
// -----------------------------------------------------------------------------

func TestFileService_AccessCRUD_AndHistory(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	u := UserForTest{Email: "u@b.com", Role: "User", FirstName: "U", LastName: "L"}

	_ = db.Create(&u).Error

	f := File{Filename: "f1", InsertedBy: uint(u.ID)}
	_ = db.Create(&f).Error

	// CreateAccess ok
	if err := svc.CreateAccess([]FileAccess{{UserID: uint(u.ID), FileID: f.ID}}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	// GetFileAccess ok
	out, err := svc.GetFileAccess(fmt.Sprintf("%d", f.ID))
	if err != nil || len(out) != 1 {
		t.Fatalf("expected 1, got %d err=%v %#v", len(out), err, out)
	}

	// DeleteAccess not found
	if err := svc.DeleteAccess("999"); err == nil {
		t.Fatalf("expected err")
	}

	// DeleteAccess ok
	var acc FileAccess
	_ = db.Where("file_id = ? AND user_id = ?", f.ID, u.ID).First(&acc).Error
	if err := svc.DeleteAccess(fmt.Sprintf("%d", acc.ID)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	var count int64
	_ = db.Model(&FileAccess{}).Where("id = ?", acc.ID).Count(&count).Error
	if count != 0 {
		t.Fatalf("expected deleted access row")
	}

	// DeleteAccess delete error: find then drop table
	_ = db.Create(&FileAccess{UserID: uint(u.ID), FileID: f.ID}).Error
	_ = db.Where("file_id = ? AND user_id = ?", f.ID, u.ID).First(&acc).Error
	_ = db.Migrator().DropTable(&FileAccess{})
	if err := svc.DeleteAccess(fmt.Sprintf("%d", acc.ID)); err == nil {
		t.Fatalf("expected delete error")
	}

	// CreateAccess error
	db = newTestDB(t)
	svc = &FileService{DB: db}
	_ = db.Migrator().DropTable(&FileAccess{})
	if err := svc.CreateAccess([]FileAccess{{UserID: 1, FileID: 1}}); err == nil {
		t.Fatalf("expected create error")
	}

	// history
	db = newTestDB(t)
	svc = &FileService{DB: db}
	u = UserForTest{Email: "u2@b.com", Role: "User", FirstName: "A", LastName: "B"}
	_ = db.Create(&u).Error
	f = File{Filename: "f2", InsertedBy: uint(u.ID)}
	_ = db.Create(&f).Error
	cols, _ := json.Marshal([]string{"a", "b"})
	_ = db.Create(&FileVersion{
		FileID:       f.ID,
		Filename:     f.Filename,
		InsertedBy:   uint(u.ID),
		Version:      1,
		Rows:         10,
		ColumnsOrder: cols,
	}).Error
	hist, err := svc.GetFileHistory(fmt.Sprintf("%d", f.ID))
	if err != nil || len(hist) != 1 {
		t.Fatalf("expected 1 history row, got %d err=%v", len(hist), err)
	}
	if string(hist[0].ColumnsOrder) != `["a","b"]` {
		t.Fatalf("expected history columns order, got %s", string(hist[0].ColumnsOrder))
	}
	_ = db.Migrator().DropTable(&FileVersion{})
	_, err = svc.GetFileHistory(fmt.Sprintf("%d", f.ID))
	if err == nil {
		t.Fatalf("expected history query err")
	}
}

func TestFileService_GetFileHistory_ReturnsDistinctVersionsWithFileDataFallback(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	originalUser := UserForTest{Email: "original@example.com", Role: "User", FirstName: "Original", LastName: "Uploader"}
	replacerUser := UserForTest{Email: "replacer@example.com", Role: "User", FirstName: "Replacement", LastName: "Uploader"}
	if err := db.Create(&originalUser).Error; err != nil {
		t.Fatalf("seed original user: %v", err)
	}
	if err := db.Create(&replacerUser).Error; err != nil {
		t.Fatalf("seed replacer user: %v", err)
	}

	cols := datatypes.JSON([]byte(`["a","b"]`))
	file := File{
		Filename:     "history-fallback.csv",
		InsertedBy:   originalUser.ID,
		Version:      2,
		Rows:         1,
		ColumnsOrder: cols,
		Size:         222.5,
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		InsertedBy:           replacerUser.ID,
		Version:              2,
		Rows:                 1,
		Size:                 222.5,
		ColumnsOrder:         cols,
		ReconciliationStatus: fileVersionStatusReady,
		TransitionOperation:  versionTransitionOperationReplace,
	}).Error; err != nil {
		t.Fatalf("seed version 2 metadata: %v", err)
	}

	if err := db.Create(&FileData{
		FileID:     file.ID,
		RowData:    datatypes.JSON([]byte(`{"a":"v1","b":"row"}`)),
		InsertedBy: originalUser.ID,
		Version:    1,
	}).Error; err != nil {
		t.Fatalf("seed version 1 row: %v", err)
	}
	if err := db.Create(&FileData{
		FileID:     file.ID,
		RowData:    datatypes.JSON([]byte(`{"a":"v2","b":"row"}`)),
		InsertedBy: replacerUser.ID,
		Version:    2,
	}).Error; err != nil {
		t.Fatalf("seed version 2 row: %v", err)
	}

	history, err := svc.GetFileHistory(fmt.Sprintf("%d", file.ID))
	if err != nil {
		t.Fatalf("get file history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 history versions, got %d", len(history))
	}

	if history[0].Version != 2 || history[1].Version != 1 {
		t.Fatalf("unexpected history order: %#v", history)
	}
	if history[0].TransitionOperation != versionTransitionOperationReplace {
		t.Fatalf("expected version 2 transition operation, got %#v", history[0])
	}
	if history[1].Firstname != originalUser.FirstName || history[1].Lastname != originalUser.LastName {
		t.Fatalf("expected fallback uploader names for version 1, got %#v", history[1])
	}
	if history[1].Rows != 1 {
		t.Fatalf("expected fallback version rows=1, got %#v", history[1])
	}
	if string(history[1].ColumnsOrder) != `["a","b"]` {
		t.Fatalf("expected fallback columns order, got %s", string(history[1].ColumnsOrder))
	}
}

func TestFileService_GetFileHistory_PrefersSingleReadySnapshotPerVersion(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	user := UserForTest{Email: "history@example.com", Role: "User", FirstName: "History", LastName: "Tester"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	cols := datatypes.JSON([]byte(`["a"]`))
	file := File{
		Filename:     "history-dedupe.csv",
		InsertedBy:   user.ID,
		Version:      1,
		Rows:         1,
		ColumnsOrder: cols,
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		InsertedBy:           user.ID,
		Version:              1,
		Rows:                 1,
		ColumnsOrder:         cols,
		ReconciliationStatus: fileVersionStatusReady,
		CreatedAt:            time.Now().Add(-2 * time.Minute),
	}).Error; err != nil {
		t.Fatalf("seed ready version row: %v", err)
	}
	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		InsertedBy:           user.ID,
		Version:              1,
		Rows:                 1,
		ColumnsOrder:         datatypes.JSON([]byte(`["wrong"]`)),
		ReconciliationStatus: fileVersionStatusProcessing,
		TransitionOperation:  versionTransitionOperationReplace,
		CreatedAt:            time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed duplicate processing version row: %v", err)
	}

	history, err := svc.GetFileHistory(fmt.Sprintf("%d", file.ID))
	if err != nil {
		t.Fatalf("get file history: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 deduped history row, got %d", len(history))
	}
	if history[0].ReconciliationStatus != fileVersionStatusReady {
		t.Fatalf("expected ready snapshot to win, got %#v", history[0])
	}
	if string(history[0].ColumnsOrder) != `["a"]` {
		t.Fatalf("expected ready snapshot columns order, got %s", string(history[0].ColumnsOrder))
	}
}

// -----------------------------------------------------------------------------
// RevertFile
// -----------------------------------------------------------------------------

func TestFileService_RevertFile_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	// file not found
	if err := svc.RevertFile("missing", 1, 1); err == nil {
		t.Fatalf("expected err")
	}

	// seed file
	f := File{Filename: "f1", Version: 2, Rows: 2, Size: 1.0, ColumnsOrder: datatypes.JSON([]byte(`["current"]`))}
	_ = db.Create(&f).Error

	// target version not found
	if err := svc.RevertFile("f1", 99, 1); err == nil {
		t.Fatalf("expected target not found")
	}

	// blocked while another transition is already queued
	if err := db.Create(&FileVersion{
		FileID:               f.ID,
		Filename:             f.Filename,
		Version:              3,
		ReconciliationStatus: fileVersionStatusProcessing,
	}).Error; err != nil {
		t.Fatalf("seed queued version: %v", err)
	}
	if err := svc.RevertFile("f1", 1, 1); !errors.Is(err, ErrFileVersionTransitionInProgress) {
		t.Fatalf("expected transition in progress err, got %v", err)
	}

	// seed target version + file_data
	_ = db.Create(&FileVersion{FileID: f.ID, Filename: "f1", Version: 1, Rows: 1, Size: 0.5, Private: false, ColumnsOrder: datatypes.JSON([]byte(`["a"]`))}).Error
	_ = db.Create(&FileData{FileID: f.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"a":"1"}`))}).Error

	// update file error: drop file table after reading
	_ = db.Migrator().DropTable(&File{})
	if err := svc.RevertFile("f1", 1, 2); err == nil {
		t.Fatalf("expected update err")
	}

	// recreate for next errors
	db = newTestDB(t)
	svc = &FileService{DB: db}
	f = File{Filename: "f1", Version: 2, Rows: 2, Size: 1.0, ColumnsOrder: datatypes.JSON([]byte(`["current"]`))}
	_ = db.Create(&f).Error
	_ = db.Create(&FileVersion{FileID: f.ID, Filename: "f1", Version: 1, Rows: 1, Size: 0.5, Private: false, ColumnsOrder: datatypes.JSON([]byte(`["a"]`))}).Error
	_ = db.Create(&FileData{FileID: f.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"a":"1"}`))}).Error

	// create new file_version error
	_ = db.Migrator().DropTable(&FileVersion{})
	if err := svc.RevertFile("f1", 1, 2); err == nil {
		t.Fatalf("expected create file_version err")
	}

	// find data rows error
	db = newTestDB(t)
	svc = &FileService{DB: db}
	f = File{Filename: "f1", Version: 2, Rows: 2, Size: 1.0, ColumnsOrder: datatypes.JSON([]byte(`["current"]`))}
	_ = db.Create(&f).Error
	_ = db.Create(&FileVersion{FileID: f.ID, Filename: "f1", Version: 1, Rows: 1, Size: 0.5, Private: false, ColumnsOrder: datatypes.JSON([]byte(`["a"]`))}).Error
	_ = db.Create(&FileData{FileID: f.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"a":"1"}`))}).Error
	_ = db.Migrator().DropTable(&FileData{})
	if err := svc.RevertFile("f1", 1, 2); err == nil {
		t.Fatalf("expected find data err")
	}

	// success
	db = newTestDB(t)
	svc = &FileService{DB: db}
	f = File{
		Filename:     "f2",
		Version:      2,
		Rows:         2,
		Size:         1.0,
		Private:      true,
		ColumnsOrder: datatypes.JSON([]byte(`["current"]`)),
	}
	_ = db.Create(&f).Error
	_ = db.Create(&FileVersion{
		FileID:       f.ID,
		Filename:     "f2",
		Version:      2,
		Rows:         1,
		Size:         0.5,
		Private:      true,
		ColumnsOrder: datatypes.JSON([]byte(`["a","b"]`)),
	}).Error
	_ = db.Create(&FileData{FileID: f.ID, Version: 2, RowData: datatypes.JSON([]byte(`{"a":"1"}`))}).Error

	if err := svc.RevertFile("f2", 2, 99); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var updated File
	_ = db.Where("filename = ?", "f2").First(&updated).Error
	if updated.Version != 2 || updated.Rows != 2 || updated.Private != true {
		t.Fatalf("unexpected queued file state: %#v", updated)
	}

	var queuedVersion FileVersion
	if err := db.Where("file_id = ? AND version = ?", f.ID, 3).First(&queuedVersion).Error; err != nil {
		t.Fatalf("expected queued version row: %v", err)
	}
	if queuedVersion.ReconciliationStatus != fileVersionStatusProcessing {
		t.Fatalf("expected queued version to be processing, got %#v", queuedVersion)
	}

	if _, err := RunVersionReconciliationJobs(db, VersionReconciliationRunOptions{MaxJobs: 1}); err != nil {
		t.Fatalf("run reconciliation jobs: %v", err)
	}

	_ = db.Where("filename = ?", "f2").First(&updated).Error
	if updated.Version != 3 || updated.Rows != 1 || updated.Private != true {
		t.Fatalf("unexpected updated file: %#v", updated)
	}
	if string(updated.ColumnsOrder) != `["a","b"]` {
		t.Fatalf("expected reverted file columns order, got %s", string(updated.ColumnsOrder))
	}
	var newRows int64
	_ = db.Model(&FileData{}).Where("file_id = ? AND version = ?", f.ID, 3).Count(&newRows).Error
	if newRows != 1 {
		t.Fatalf("expected copied 1 row, got %d", newRows)
	}
	var latestVersion FileVersion
	if err := db.Where("file_id = ? AND version = ?", f.ID, 3).First(&latestVersion).Error; err != nil {
		t.Fatalf("expected reverted version row: %v", err)
	}
	if string(latestVersion.ColumnsOrder) != `["a","b"]` {
		t.Fatalf("expected reverted version columns order, got %s", string(latestVersion.ColumnsOrder))
	}
}

func TestRevertFile_ClearsDailyHonours(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}
	file := File{
		Filename: "f2",
		Version:  2,
		Rows:     1,
		Size:     1.0,
		Private:  true,
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("create file: %v", err)
	}
	if err := db.Create(&FileVersion{
		FileID:   file.ID,
		Filename: "f2",
		Version:  2,
		Rows:     1,
		Size:     0.5,
		Private:  true,
	}).Error; err != nil {
		t.Fatalf("create file version: %v", err)
	}
	row := FileData{FileID: file.ID, Version: 2, RowData: datatypes.JSON([]byte(`{"a":"1"}`))}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("create row: %v", err)
	}
	if err := db.Create(&honour.DailyHonour{
		FileID:      file.ID,
		FileVersion: 2,
		SourceRowID: row.ID,
		HonourDate:  time.Date(2026, time.July, 14, 0, 0, 0, 0, time.UTC),
		CycleNumber: 1,
		HonourText:  "Remembered today",
		Status:      "ready",
	}).Error; err != nil {
		t.Fatalf("create honour: %v", err)
	}

	if err := svc.RevertFile("f2", 2, 99); err != nil {
		t.Fatalf("revert file: %v", err)
	}

	var count int64
	if err := db.Model(&honour.DailyHonour{}).Where("file_id = ?", file.ID).Count(&count).Error; err != nil {
		t.Fatalf("count honours: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected honours to be cleared, got %d", count)
	}
}

func TestFileService_RevertFile_UsesNextUnusedVersionNumber(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	file := File{
		Filename:     "revert-gap.csv",
		Version:      5,
		Rows:         1,
		ColumnsOrder: datatypes.JSON([]byte(`["name"]`)),
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		Version:              1,
		Rows:                 1,
		ColumnsOrder:         datatypes.JSON([]byte(`["name"]`)),
		ReconciliationStatus: fileVersionStatusReady,
	}).Error; err != nil {
		t.Fatalf("seed target version: %v", err)
	}
	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		Version:              6,
		Rows:                 1,
		ColumnsOrder:         datatypes.JSON([]byte(`["name"]`)),
		ReconciliationStatus: fileVersionStatusReady,
	}).Error; err != nil {
		t.Fatalf("seed higher historical version: %v", err)
	}
	if err := db.Create(&FileData{
		FileID:  file.ID,
		Version: 1,
		RowData: datatypes.JSON([]byte(`{"name":"older"}`)),
	}).Error; err != nil {
		t.Fatalf("seed target data row: %v", err)
	}
	if err := db.Create(&FileData{
		FileID:  file.ID,
		Version: 6,
		RowData: datatypes.JSON([]byte(`{"name":"existing-v6"}`)),
	}).Error; err != nil {
		t.Fatalf("seed existing higher-version row: %v", err)
	}

	if err := svc.RevertFile(file.Filename, 1, 77); err != nil {
		t.Fatalf("queue revert: %v", err)
	}

	var queuedVersion FileVersion
	if err := db.Where("file_id = ? AND transition_operation = ?", file.ID, versionTransitionOperationRevert).
		Order("id DESC").
		First(&queuedVersion).Error; err != nil {
		t.Fatalf("load queued version: %v", err)
	}
	if queuedVersion.Version != 7 {
		t.Fatalf("expected revert to allocate version 7, got %d", queuedVersion.Version)
	}

	var job FileVersionReconciliationJob
	if err := db.Where("file_id = ?", file.ID).Order("id DESC").First(&job).Error; err != nil {
		t.Fatalf("load queued job: %v", err)
	}
	if job.TargetVersion != 7 {
		t.Fatalf("expected job target version 7, got %#v", job)
	}
}

func TestFileService_ReviewEditRequest_AddInformationUsesCurrentVersionAndUpdatesColumnsOrder(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}

	prevMove := moveGCSFolderHook
	t.Cleanup(func() { moveGCSFolderHook = prevMove })
	moveGCSFolderHook = func(bucket, src, dst string) (map[string]string, error) {
		return map[string]string{}, nil
	}

	file := File{
		Filename:     "people.csv",
		Version:      3,
		Rows:         0,
		ColumnsOrder: datatypes.JSON([]byte(`["First Names","Last Names"]`)),
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		Version:              3,
		Rows:                 0,
		ColumnsOrder:         datatypes.JSON([]byte(`["First Names","Last Names"]`)),
		ReconciliationStatus: fileVersionStatusReady,
	}).Error; err != nil {
		t.Fatalf("seed file version: %v", err)
	}

	req := FileEditRequest{
		UserID:    1,
		Status:    "pending",
		IsEdited:  false,
		FirstName: "Athul",
		LastName:  "N",
		RowID:     0,
		FileID:    file.ID,
	}
	if err := db.Create(&req).Error; err != nil {
		t.Fatalf("seed request: %v", err)
	}
	details := []FileEditRequestDetails{
		{RequestID: req.RequestID, FileID: file.ID, Filename: file.Filename, RowID: 0, FieldName: "First Names", NewValue: "Athul"},
		{RequestID: req.RequestID, FileID: file.ID, Filename: file.Filename, RowID: 0, FieldName: "Last Names", NewValue: "N"},
		{RequestID: req.RequestID, FileID: file.ID, Filename: file.Filename, RowID: 0, FieldName: "Middle Names", NewValue: "S"},
	}
	if err := db.Create(&details).Error; err != nil {
		t.Fatalf("seed details: %v", err)
	}

	if err := svc.ReviewEditRequest(req.RequestID, "completed", "looks good", nil, 9); err != nil {
		t.Fatalf("review request: %v", err)
	}

	var addedRows []FileData
	if err := db.Where("file_id = ? AND version = ?", file.ID, 3).Order("id ASC").Find(&addedRows).Error; err != nil {
		t.Fatalf("load added rows: %v", err)
	}
	if len(addedRows) != 1 {
		t.Fatalf("expected 1 current-version added row, got %d", len(addedRows))
	}

	var row map[string]string
	if err := json.Unmarshal(addedRows[0].RowData, &row); err != nil {
		t.Fatalf("unmarshal added row: %v", err)
	}
	if row["Middle Names"] != "S" {
		t.Fatalf("expected middle name in added row, got %#v", row)
	}

	var updatedFile File
	if err := db.First(&updatedFile, file.ID).Error; err != nil {
		t.Fatalf("reload file: %v", err)
	}
	if updatedFile.Rows != 1 {
		t.Fatalf("expected file rows to update, got %#v", updatedFile)
	}
	if string(updatedFile.ColumnsOrder) != `["First Names","Last Names","Middle Names"]` {
		t.Fatalf("unexpected file columns order: %s", string(updatedFile.ColumnsOrder))
	}

	var updatedVersion FileVersion
	if err := db.Where("file_id = ? AND version = ?", file.ID, 3).First(&updatedVersion).Error; err != nil {
		t.Fatalf("reload file version: %v", err)
	}
	if updatedVersion.Rows != 1 {
		t.Fatalf("expected file version rows to update, got %#v", updatedVersion)
	}
	if string(updatedVersion.ColumnsOrder) != `["First Names","Last Names","Middle Names"]` {
		t.Fatalf("unexpected file version columns order: %s", string(updatedVersion.ColumnsOrder))
	}
}

func TestFileService_VersionReconciliation_ReplaceAndRevert(t *testing.T) {
	t.Run("replace matches duplicate names by school and carries forward legacy rows", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{
			Filename:     "people.csv",
			Version:      1,
			Rows:         3,
			ColumnsOrder: datatypes.JSON([]byte(`["First Names","Last Names","School","Middle Names"]`)),
		}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FileVersion{
			FileID:               file.ID,
			Filename:             file.Filename,
			Version:              1,
			Rows:                 3,
			ColumnsOrder:         file.ColumnsOrder,
			ReconciliationStatus: fileVersionStatusReady,
		}).Error; err != nil {
			t.Fatalf("seed current version: %v", err)
		}

		alphaRow := FileData{FileID: file.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"First Names":"Athul","Last Names":"N","School":"Alpha","Middle Names":"s"}`))}
		betaRow := FileData{FileID: file.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"First Names":"Athul","Last Names":"N","School":"Beta"}`))}
		legacyRow := FileData{FileID: file.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"First Names":"Legacy","Last Names":"Person"}`))}
		for _, row := range []*FileData{&alphaRow, &betaRow, &legacyRow} {
			if err := db.Create(row).Error; err != nil {
				t.Fatalf("seed file data: %v", err)
			}
		}

		editReq := FileEditRequest{UserID: 1, Status: "completed", IsEdited: true, RowID: int(alphaRow.ID), FileID: file.ID}
		if err := db.Create(&editReq).Error; err != nil {
			t.Fatalf("seed edit request: %v", err)
		}
		if err := db.Create(&FileEditRequestDetails{
			RequestID: editReq.RequestID,
			FileID:    file.ID,
			Filename:  file.Filename,
			RowID:     int(alphaRow.ID),
			FieldName: "Middle Names",
			NewValue:  "s",
			Status:    "approved",
		}).Error; err != nil {
			t.Fatalf("seed edit detail: %v", err)
		}

		legacyReq := FileEditRequest{UserID: 1, Status: "completed", IsEdited: false, RowID: int(legacyRow.ID), FileID: file.ID, FirstName: "Legacy", LastName: "Person"}
		if err := db.Create(&legacyReq).Error; err != nil {
			t.Fatalf("seed legacy request: %v", err)
		}
		if err := db.Create(&FileEditRequestDetails{
			RequestID: legacyReq.RequestID,
			FileID:    file.ID,
			Filename:  file.Filename,
			RowID:     int(legacyRow.ID),
			FieldName: "First Names",
			NewValue:  "Legacy",
			Status:    "approved",
		}).Error; err != nil {
			t.Fatalf("seed legacy detail: %v", err)
		}

		photo := FileEditRequestPhoto{RequestID: editReq.RequestID, RowID: int(betaRow.ID), FileID: file.ID, PhotoURL: "gs://bucket/photos/beta.jpg", FileName: "beta.jpg", DocumentType: "photos"}
		if err := db.Create(&photo).Error; err != nil {
			t.Fatalf("seed photo: %v", err)
		}

		form := FormSubmissionForTest{FileID: file.ID, RowID: int64(betaRow.ID), FileName: file.Filename, Status: "rejected"}
		if err := db.Create(&form).Error; err != nil {
			t.Fatalf("seed form submission: %v", err)
		}

		fh := fileHeaderFromBytes(t, "file", "people.csv", csvBytes(
			[]string{"Last Names", "First Names", "School"},
			[][]string{
				{"N", "Athul", "Beta"},
				{"N", "Athul", "Alpha"},
			},
		))
		if err := svc.ReplaceFiles(fh, file.ID, 42); err != nil {
			t.Fatalf("queue replace: %v", err)
		}

		if _, err := RunVersionReconciliationJobs(db, VersionReconciliationRunOptions{MaxJobs: 1}); err != nil {
			t.Fatalf("run reconciliation jobs: %v", err)
		}

		var updatedFile File
		if err := db.First(&updatedFile, file.ID).Error; err != nil {
			t.Fatalf("reload file: %v", err)
		}
		if updatedFile.Version != 2 || updatedFile.Rows != 3 {
			t.Fatalf("unexpected reconciled file: %#v", updatedFile)
		}
		if string(updatedFile.ColumnsOrder) != `["Last Names","First Names","School","Middle Names"]` {
			t.Fatalf("unexpected reconciled columns order: %s", string(updatedFile.ColumnsOrder))
		}

		var versionTwoRows []FileData
		if err := db.Where("file_id = ? AND version = ?", file.ID, 2).Order("id ASC").Find(&versionTwoRows).Error; err != nil {
			t.Fatalf("load version 2 rows: %v", err)
		}
		if len(versionTwoRows) != 3 {
			t.Fatalf("expected 3 version 2 rows, got %d", len(versionTwoRows))
		}

		rowBySchool := make(map[string]FileData)
		var legacyVersionTwoRow FileData
		for _, row := range versionTwoRows {
			var payload map[string]string
			if err := json.Unmarshal(row.RowData, &payload); err != nil {
				t.Fatalf("unmarshal reconciled row: %v", err)
			}
			if payload["School"] != "" {
				rowBySchool[payload["School"]] = row
			}
			if payload["First Names"] == "Legacy" && payload["Last Names"] == "Person" {
				legacyVersionTwoRow = row
			}
			if payload["School"] == "Alpha" && payload["Middle Names"] != "s" {
				t.Fatalf("expected approved middle name to be replayed, got %#v", payload)
			}
		}
		if rowBySchool["Beta"].ID == 0 || rowBySchool["Alpha"].ID == 0 {
			t.Fatalf("expected both duplicate-name rows to remain in version 2")
		}
		if legacyVersionTwoRow.ID == 0 {
			t.Fatalf("expected legacy row to be carried forward")
		}

		var updatedPhoto FileEditRequestPhoto
		if err := db.First(&updatedPhoto, photo.ID).Error; err != nil {
			t.Fatalf("reload photo: %v", err)
		}
		if updatedPhoto.RowID != int(rowBySchool["Beta"].ID) {
			t.Fatalf("expected photo to relink to beta row, got %#v", updatedPhoto)
		}

		var updatedForm FormSubmissionForTest
		if err := db.First(&updatedForm, form.ID).Error; err != nil {
			t.Fatalf("reload form submission: %v", err)
		}
		if updatedForm.RowID != int64(rowBySchool["Beta"].ID) || updatedForm.FileName != file.Filename {
			t.Fatalf("expected form submission to relink to beta row, got %#v", updatedForm)
		}

		var updatedLegacyReq FileEditRequest
		if err := db.First(&updatedLegacyReq, legacyReq.RequestID).Error; err != nil {
			t.Fatalf("reload legacy request: %v", err)
		}
		if updatedLegacyReq.RowID != int(legacyVersionTwoRow.ID) {
			t.Fatalf("expected legacy request to relink to carried row, got %#v", updatedLegacyReq)
		}

		var lineageCount int64
		if err := db.Model(&FileRowLineage{}).Where("file_id = ? AND target_version = ?", file.ID, 2).Count(&lineageCount).Error; err != nil {
			t.Fatalf("count lineage rows: %v", err)
		}
		if lineageCount != 3 {
			t.Fatalf("expected lineage for all source rows, got %d", lineageCount)
		}
	})

	t.Run("revert replays approved edits and carries forward legacy rows", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{
			Filename:     "people.csv",
			Version:      2,
			Rows:         3,
			ColumnsOrder: datatypes.JSON([]byte(`["First Names","Last Names","School","Middle Names"]`)),
		}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FileVersion{
			FileID:               file.ID,
			Filename:             file.Filename,
			Version:              1,
			Rows:                 2,
			ColumnsOrder:         datatypes.JSON([]byte(`["First Names","Last Names","School"]`)),
			ReconciliationStatus: fileVersionStatusReady,
		}).Error; err != nil {
			t.Fatalf("seed historical version: %v", err)
		}
		if err := db.Create(&FileVersion{
			FileID:               file.ID,
			Filename:             file.Filename,
			Version:              2,
			Rows:                 3,
			ColumnsOrder:         file.ColumnsOrder,
			ReconciliationStatus: fileVersionStatusReady,
		}).Error; err != nil {
			t.Fatalf("seed current version: %v", err)
		}

		_ = db.Create(&FileData{FileID: file.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"First Names":"Athul","Last Names":"N","School":"Alpha"}`))}).Error
		_ = db.Create(&FileData{FileID: file.ID, Version: 1, RowData: datatypes.JSON([]byte(`{"First Names":"Athul","Last Names":"N","School":"Beta"}`))}).Error

		alphaCurrent := FileData{FileID: file.ID, Version: 2, RowData: datatypes.JSON([]byte(`{"First Names":"Athul","Last Names":"N","School":"Alpha","Middle Names":"s"}`))}
		betaCurrent := FileData{FileID: file.ID, Version: 2, RowData: datatypes.JSON([]byte(`{"First Names":"Athul","Last Names":"N","School":"Beta"}`))}
		legacyCurrent := FileData{FileID: file.ID, Version: 2, RowData: datatypes.JSON([]byte(`{"First Names":"Legacy","Last Names":"Person"}`))}
		for _, row := range []*FileData{&alphaCurrent, &betaCurrent, &legacyCurrent} {
			if err := db.Create(row).Error; err != nil {
				t.Fatalf("seed current row: %v", err)
			}
		}

		editReq := FileEditRequest{UserID: 1, Status: "completed", IsEdited: true, RowID: int(alphaCurrent.ID), FileID: file.ID}
		if err := db.Create(&editReq).Error; err != nil {
			t.Fatalf("seed edit request: %v", err)
		}
		if err := db.Create(&FileEditRequestDetails{
			RequestID: editReq.RequestID,
			FileID:    file.ID,
			Filename:  file.Filename,
			RowID:     int(alphaCurrent.ID),
			FieldName: "Middle Names",
			NewValue:  "s",
			Status:    "approved",
		}).Error; err != nil {
			t.Fatalf("seed edit detail: %v", err)
		}

		legacyReq := FileEditRequest{UserID: 1, Status: "completed", IsEdited: false, RowID: int(legacyCurrent.ID), FileID: file.ID, FirstName: "Legacy", LastName: "Person"}
		if err := db.Create(&legacyReq).Error; err != nil {
			t.Fatalf("seed legacy request: %v", err)
		}
		if err := db.Create(&FileEditRequestDetails{
			RequestID: legacyReq.RequestID,
			FileID:    file.ID,
			Filename:  file.Filename,
			RowID:     int(legacyCurrent.ID),
			FieldName: "First Names",
			NewValue:  "Legacy",
			Status:    "approved",
		}).Error; err != nil {
			t.Fatalf("seed legacy detail: %v", err)
		}

		photo := FileEditRequestPhoto{RequestID: editReq.RequestID, RowID: int(betaCurrent.ID), FileID: file.ID, PhotoURL: "gs://bucket/photos/beta.jpg", FileName: "beta.jpg", DocumentType: "photos"}
		if err := db.Create(&photo).Error; err != nil {
			t.Fatalf("seed photo: %v", err)
		}

		if err := svc.RevertFile(file.Filename, 1, 77); err != nil {
			t.Fatalf("queue revert: %v", err)
		}
		if _, err := RunVersionReconciliationJobs(db, VersionReconciliationRunOptions{MaxJobs: 1}); err != nil {
			t.Fatalf("run reconciliation jobs: %v", err)
		}

		var updatedFile File
		if err := db.First(&updatedFile, file.ID).Error; err != nil {
			t.Fatalf("reload file: %v", err)
		}
		if updatedFile.Version != 3 || updatedFile.Rows != 3 {
			t.Fatalf("unexpected reverted file: %#v", updatedFile)
		}

		var versionThreeRows []FileData
		if err := db.Where("file_id = ? AND version = ?", file.ID, 3).Order("id ASC").Find(&versionThreeRows).Error; err != nil {
			t.Fatalf("load version 3 rows: %v", err)
		}
		if len(versionThreeRows) != 3 {
			t.Fatalf("expected 3 version 3 rows, got %d", len(versionThreeRows))
		}

		rowBySchool := make(map[string]FileData)
		var legacyVersionThreeRow FileData
		for _, row := range versionThreeRows {
			var payload map[string]string
			if err := json.Unmarshal(row.RowData, &payload); err != nil {
				t.Fatalf("unmarshal reverted row: %v", err)
			}
			if payload["School"] != "" {
				rowBySchool[payload["School"]] = row
			}
			if payload["First Names"] == "Legacy" && payload["Last Names"] == "Person" {
				legacyVersionThreeRow = row
			}
			if payload["School"] == "Alpha" && payload["Middle Names"] != "s" {
				t.Fatalf("expected approved middle name after revert, got %#v", payload)
			}
		}
		if legacyVersionThreeRow.ID == 0 {
			t.Fatalf("expected legacy row to be carried forward on revert")
		}

		var updatedPhoto FileEditRequestPhoto
		if err := db.First(&updatedPhoto, photo.ID).Error; err != nil {
			t.Fatalf("reload photo: %v", err)
		}
		if updatedPhoto.RowID != int(rowBySchool["Beta"].ID) {
			t.Fatalf("expected photo to relink to beta row after revert, got %#v", updatedPhoto)
		}

		var updatedLegacyReq FileEditRequest
		if err := db.First(&updatedLegacyReq, legacyReq.RequestID).Error; err != nil {
			t.Fatalf("reload legacy request: %v", err)
		}
		if updatedLegacyReq.RowID != int(legacyVersionThreeRow.ID) {
			t.Fatalf("expected legacy request to relink after revert, got %#v", updatedLegacyReq)
		}
	})
}

func TestRunVersionReconciliationJobs_RecoversStaleProcessingJob(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	file := File{
		Filename:     "stale-reconciliation.csv",
		Version:      1,
		Rows:         1,
		ColumnsOrder: datatypes.JSON([]byte(`["name"]`)),
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		Version:              1,
		Rows:                 1,
		ColumnsOrder:         datatypes.JSON([]byte(`["name"]`)),
		ReconciliationStatus: fileVersionStatusReady,
	}).Error; err != nil {
		t.Fatalf("seed current version: %v", err)
	}
	if err := db.Create(&FileData{
		FileID:  file.ID,
		Version: 1,
		RowData: datatypes.JSON([]byte(`{"name":"before"}`)),
	}).Error; err != nil {
		t.Fatalf("seed current row: %v", err)
	}

	fh := fileHeaderFromBytes(t, "file", "stale.csv", csvBytes(
		[]string{"name"},
		[][]string{{"after"}},
	))
	if err := svc.ReplaceFiles(fh, file.ID, 11); err != nil {
		t.Fatalf("queue replace: %v", err)
	}

	var job FileVersionReconciliationJob
	if err := db.Where("file_id = ?", file.ID).First(&job).Error; err != nil {
		t.Fatalf("load queued job: %v", err)
	}
	staleAt := time.Now().Add(-defaultVersionReconciliationLeaseTTL - time.Minute)
	if err := db.Model(&FileVersionReconciliationJob{}).
		Where("id = ?", job.ID).
		UpdateColumns(map[string]any{
			"status":       reconciliationJobStatusProcessing,
			"attempts":     1,
			"started_at":   staleAt,
			"updated_at":   staleAt,
			"available_at": staleAt,
		}).Error; err != nil {
		t.Fatalf("mark job stale: %v", err)
	}

	result, err := RunVersionReconciliationJobs(db, VersionReconciliationRunOptions{MaxJobs: 1})
	if err != nil {
		t.Fatalf("run reconciliation jobs: %v", err)
	}
	if result.Claimed != 1 || result.Completed != 1 {
		t.Fatalf("expected recovered job to complete in same run, got %#v", result)
	}

	if err := db.First(&job, job.ID).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if job.Status != reconciliationJobStatusCompleted {
		t.Fatalf("expected stale job to complete after recovery, got %#v", job)
	}
	if job.Attempts != 2 {
		t.Fatalf("expected stale job to be reclaimed with attempts=2, got %#v", job)
	}

	var updatedFile File
	if err := db.First(&updatedFile, file.ID).Error; err != nil {
		t.Fatalf("reload file: %v", err)
	}
	if updatedFile.Version != 2 || updatedFile.Rows != 1 {
		t.Fatalf("unexpected reconciled file after stale recovery: %#v", updatedFile)
	}

	var versionTwo FileVersion
	if err := db.Where("file_id = ? AND version = ?", file.ID, 2).First(&versionTwo).Error; err != nil {
		t.Fatalf("reload reconciled version: %v", err)
	}
	if versionTwo.ReconciliationStatus != fileVersionStatusReady {
		t.Fatalf("expected reconciled version to be ready, got %#v", versionTwo)
	}
}

func TestRunVersionReconciliationJobs_DoesNotRecoverProcessingJobWithRecentHeartbeat(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	file := File{
		Filename:     "active-heartbeat.csv",
		Version:      1,
		Rows:         1,
		ColumnsOrder: datatypes.JSON([]byte(`["name"]`)),
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		Version:              1,
		Rows:                 1,
		ColumnsOrder:         datatypes.JSON([]byte(`["name"]`)),
		ReconciliationStatus: fileVersionStatusReady,
	}).Error; err != nil {
		t.Fatalf("seed current version: %v", err)
	}
	if err := db.Create(&FileData{
		FileID:  file.ID,
		Version: 1,
		RowData: datatypes.JSON([]byte(`{"name":"before"}`)),
	}).Error; err != nil {
		t.Fatalf("seed current row: %v", err)
	}

	fh := fileHeaderFromBytes(t, "file", "active-heartbeat.csv", csvBytes(
		[]string{"name"},
		[][]string{{"after"}},
	))
	if err := svc.ReplaceFiles(fh, file.ID, 11); err != nil {
		t.Fatalf("queue replace: %v", err)
	}

	var job FileVersionReconciliationJob
	if err := db.Where("file_id = ?", file.ID).First(&job).Error; err != nil {
		t.Fatalf("load queued job: %v", err)
	}

	var targetVersion FileVersion
	if err := db.First(&targetVersion, job.FileVersionID).Error; err != nil {
		t.Fatalf("load target version: %v", err)
	}

	staleStartedAt := time.Now().Add(-defaultVersionReconciliationLeaseTTL - time.Minute)
	heartbeatAt := time.Now()
	if err := db.Model(&FileVersionReconciliationJob{}).
		Where("id = ?", job.ID).
		UpdateColumns(map[string]any{
			"status":       reconciliationJobStatusProcessing,
			"attempts":     1,
			"started_at":   staleStartedAt,
			"updated_at":   heartbeatAt,
			"available_at": staleStartedAt,
		}).Error; err != nil {
		t.Fatalf("mark job as actively heartbeating: %v", err)
	}

	result, err := RunVersionReconciliationJobs(db, VersionReconciliationRunOptions{MaxJobs: 1})
	if err != nil {
		t.Fatalf("run reconciliation jobs: %v", err)
	}
	if result.Claimed != 0 || result.Completed != 0 || result.Retried != 0 || result.Failed != 0 {
		t.Fatalf("expected active processing job to be left untouched, got %#v", result)
	}

	if err := db.First(&job, job.ID).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if job.Status != reconciliationJobStatusProcessing {
		t.Fatalf("expected active job to remain processing, got %#v", job)
	}
	if job.Attempts != 1 {
		t.Fatalf("expected attempts to remain unchanged, got %#v", job)
	}
	if job.LastError != "" {
		t.Fatalf("expected no recovery error on active job, got %#v", job)
	}

	if err := db.First(&targetVersion, targetVersion.ID).Error; err != nil {
		t.Fatalf("reload target version: %v", err)
	}
	if targetVersion.ReconciliationStatus != fileVersionStatusProcessing {
		t.Fatalf("expected target version to remain processing, got %#v", targetVersion)
	}
}

func TestRunVersionReconciliationJobs_FailsConflictingTargetVersionJob(t *testing.T) {
	db := newTestDB(t)

	file := File{
		Filename:     "conflicting-target-version.csv",
		Version:      5,
		Rows:         1,
		ColumnsOrder: datatypes.JSON([]byte(`["name"]`)),
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := db.Create(&FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		Version:              6,
		Rows:                 1,
		ColumnsOrder:         datatypes.JSON([]byte(`["name"]`)),
		ReconciliationStatus: fileVersionStatusReady,
	}).Error; err != nil {
		t.Fatalf("seed existing target version: %v", err)
	}

	conflictingVersion := FileVersion{
		FileID:               file.ID,
		Filename:             file.Filename,
		Version:              6,
		Rows:                 1,
		ColumnsOrder:         datatypes.JSON([]byte(`["name"]`)),
		ReconciliationStatus: fileVersionStatusProcessing,
		TransitionOperation:  versionTransitionOperationRevert,
	}
	if err := db.Create(&conflictingVersion).Error; err != nil {
		t.Fatalf("seed conflicting transition row: %v", err)
	}

	job := FileVersionReconciliationJob{
		FileID:              file.ID,
		FileVersionID:       conflictingVersion.ID,
		SourceVersion:       5,
		TargetVersion:       6,
		MaterializedVersion: 1,
		RequestedBy:         1,
		Operation:           versionTransitionOperationRevert,
		Status:              reconciliationJobStatusPending,
		AvailableAt:         time.Now(),
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatalf("seed conflicting job: %v", err)
	}

	result, err := RunVersionReconciliationJobs(db, VersionReconciliationRunOptions{MaxJobs: 1})
	if err != nil {
		t.Fatalf("run reconciliation jobs: %v", err)
	}
	if result.Claimed != 1 || result.Failed != 1 {
		t.Fatalf("expected conflicting job to fail immediately, got %#v", result)
	}

	if err := db.First(&job, job.ID).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if job.Status != reconciliationJobStatusFailed {
		t.Fatalf("expected job to be terminally failed, got %#v", job)
	}
	if !strings.Contains(job.LastError, "target version 6 conflicts") {
		t.Fatalf("expected conflict error in job, got %#v", job)
	}

	if err := db.First(&conflictingVersion, conflictingVersion.ID).Error; err != nil {
		t.Fatalf("reload conflicting version row: %v", err)
	}
	if conflictingVersion.ReconciliationStatus != fileVersionStatusFailed {
		t.Fatalf("expected conflicting version row to fail, got %#v", conflictingVersion)
	}
}

func TestFileService_BlocksFileMutationsWhenOpenRequestsExist(t *testing.T) {
	t.Run("failed historical transition does not block replace", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{Filename: "replace-failed-transition.csv", Version: 1}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FileVersion{
			FileID:               file.ID,
			Filename:             file.Filename,
			Version:              2,
			ReconciliationStatus: fileVersionStatusFailed,
			ReconciliationError:  "old failed transition",
		}).Error; err != nil {
			t.Fatalf("seed failed transition row: %v", err)
		}

		fh := fileHeaderFromBytes(t, "file", "replace.csv", csvBytes([]string{"a"}, [][]string{{"1"}}))
		if err := svc.ReplaceFiles(fh, file.ID, 1); err != nil {
			t.Fatalf("expected failed historical transition not to block replace, got %v", err)
		}
	})

	t.Run("replace blocked by pending file edit request", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{Filename: "replace-guard.csv", Version: 1}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FileEditRequest{
			UserID: 1,
			RowID:  1,
			Status: "pending",
			FileID: file.ID,
		}).Error; err != nil {
			t.Fatalf("seed file edit request: %v", err)
		}

		fh := fileHeaderFromBytes(t, "file", "replace.csv", csvBytes([]string{"a"}, [][]string{{"1"}}))
		err := svc.ReplaceFiles(fh, file.ID, 1)
		if !errors.Is(err, ErrFileOperationBlocked) {
			t.Fatalf("expected open request block, got %v", err)
		}
		if !strings.Contains(err.Error(), "open file edit request") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("replace blocked by open form submission request", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{Filename: "replace-form-guard.csv", Version: 1}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FormSubmissionForTest{
			FileID: file.ID,
			Status: "needs more information",
		}).Error; err != nil {
			t.Fatalf("seed form submission: %v", err)
		}

		fh := fileHeaderFromBytes(t, "file", "replace.csv", csvBytes([]string{"a"}, [][]string{{"1"}}))
		err := svc.ReplaceFiles(fh, file.ID, 1)
		if !errors.Is(err, ErrFileOperationBlocked) {
			t.Fatalf("expected open request block, got %v", err)
		}
		if !strings.Contains(err.Error(), "open form submission request") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("replace allowed when form submission is already approved", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{Filename: "replace-approved-form-guard.csv", Version: 1}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FormSubmissionForTest{
			FileID: file.ID,
			Status: " Approved ",
		}).Error; err != nil {
			t.Fatalf("seed form submission: %v", err)
		}

		fh := fileHeaderFromBytes(t, "file", "replace.csv", csvBytes([]string{"a"}, [][]string{{"1"}}))
		err := svc.ReplaceFiles(fh, file.ID, 1)
		if err != nil {
			t.Fatalf("expected approved submission not to block replace, got %v", err)
		}
	})

	t.Run("revert blocked by pending file edit request", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{Filename: "revert-guard.csv", Version: 1}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FileEditRequest{
			UserID: 1,
			RowID:  1,
			Status: "pending",
			FileID: file.ID,
		}).Error; err != nil {
			t.Fatalf("seed file edit request: %v", err)
		}

		err := svc.RevertFile(file.Filename, 1, 1)
		if !errors.Is(err, ErrFileOperationBlocked) {
			t.Fatalf("expected open request block, got %v", err)
		}
		if !strings.Contains(err.Error(), "open file edit request") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("revert blocked by open form submission request", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{Filename: "revert-form-guard.csv", Version: 1}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FormSubmissionForTest{
			FileID: file.ID,
			Status: "pending",
		}).Error; err != nil {
			t.Fatalf("seed form submission: %v", err)
		}

		err := svc.RevertFile(file.Filename, 1, 1)
		if !errors.Is(err, ErrFileOperationBlocked) {
			t.Fatalf("expected open request block, got %v", err)
		}
		if !strings.Contains(err.Error(), "open form submission request") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("revert allowed when form submissions are approved or rejected", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{Filename: "revert-reviewed-form-guard.csv", Version: 1}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FileVersion{
			FileID:               file.ID,
			Filename:             file.Filename,
			Version:              1,
			Rows:                 1,
			ColumnsOrder:         datatypes.JSON([]byte(`["a"]`)),
			ReconciliationStatus: fileVersionStatusReady,
		}).Error; err != nil {
			t.Fatalf("seed file version: %v", err)
		}
		if err := db.Create(&FileData{
			FileID:  file.ID,
			Version: 1,
			RowData: datatypes.JSON([]byte(`{"a":"1"}`)),
		}).Error; err != nil {
			t.Fatalf("seed file data: %v", err)
		}
		if err := db.Create(&[]FormSubmissionForTest{
			{FileID: file.ID, Status: "approved"},
			{FileID: file.ID, Status: " rejected "},
		}).Error; err != nil {
			t.Fatalf("seed reviewed form submissions: %v", err)
		}

		err := svc.RevertFile(file.Filename, 1, 1)
		if err != nil {
			t.Fatalf("expected reviewed submissions not to block revert, got %v", err)
		}
	})

	t.Run("revert allowed when file edit request is already approved", func(t *testing.T) {
		db := newTestDB(t)
		svc := &FileService{DB: db}

		file := File{Filename: "revert-approved-edit-guard.csv", Version: 1}
		if err := db.Create(&file).Error; err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := db.Create(&FileVersion{
			FileID:               file.ID,
			Filename:             file.Filename,
			Version:              1,
			Rows:                 1,
			ColumnsOrder:         datatypes.JSON([]byte(`["a"]`)),
			ReconciliationStatus: fileVersionStatusReady,
		}).Error; err != nil {
			t.Fatalf("seed file version: %v", err)
		}
		if err := db.Create(&FileData{
			FileID:  file.ID,
			Version: 1,
			RowData: datatypes.JSON([]byte(`{"a":"1"}`)),
		}).Error; err != nil {
			t.Fatalf("seed file data: %v", err)
		}
		if err := db.Create(&FileEditRequest{
			UserID: 1,
			RowID:  1,
			Status: "approved",
			FileID: file.ID,
		}).Error; err != nil {
			t.Fatalf("seed file edit request: %v", err)
		}

		err := svc.RevertFile(file.Filename, 1, 1)
		if err != nil {
			t.Fatalf("expected approved file edit request not to block revert, got %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// CreateEditRequest
// -----------------------------------------------------------------------------

func TestFileService_CreateEditRequest_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	// Hook uploadToGCS to avoid real GCS and to capture object paths
	prevUpload := uploadToGCSHook
	t.Cleanup(func() { uploadToGCSHook = prevUpload })

	var uploadedPaths []string
	uploadToGCSHook = func(base64, bucket, objectPath string) (string, int64, error) {
		uploadedPaths = append(uploadedPaths, objectPath)
		return "gs://" + bucket + "/" + objectPath, 123, nil
	}

	// main request insert error
	_ = db.Migrator().DropTable(&FileEditRequest{})
	_, err := svc.CreateEditRequest(EditRequestInput{FirstName: "A", LastName: "B", FileID: 1, Filename: "f"}, 1)
	if err == nil {
		t.Fatalf("expected create request err")
	}

	// details insert error
	db = newTestDB(t)
	svc = &FileService{DB: db}
	uploadToGCSHook = func(base64, bucket, objectPath string) (string, int64, error) {
		return "gs://" + bucket + "/" + objectPath, 123, nil
	}
	_ = db.Migrator().DropTable(&FileEditRequestDetails{})
	_, err = svc.CreateEditRequest(EditRequestInput{
		FirstName: "A", LastName: "B",
		FileID:   1,
		Filename: "f",
		Changes: map[string][]EditChangeInput{
			"block": {{RowID: 0, FieldName: "x", OldValue: "o", NewValue: "n"}},
		},
	}, 1)
	if err == nil {
		t.Fatalf("expected details insert err")
	}

	// upload error
	db = newTestDB(t)
	svc = &FileService{DB: db}
	uploadToGCSHook = func(_, _, _ string) (string, int64, error) {
		return "", 0, errors.New("upload fail")
	}
	_, err = svc.CreateEditRequest(EditRequestInput{
		FirstName: "A",
		LastName:  "B",
		FileID:    1,
		Filename:  "f",
		PhotosInApp: []PhotoInput{
			{Filename: "a.jpg", MimeType: "image/jpeg", DataBase64: "b64", Comment: "c"},
		},
		IsEdited: false,
		RowID:    0,
	}, 1)
	if err == nil {
		t.Fatalf("expected upload err")
	}

	// photo record insert error
	db = newTestDB(t)
	svc = &FileService{DB: db}
	uploadToGCSHook = func(base64, bucket, objectPath string) (string, int64, error) {
		return "gs://" + bucket + "/" + objectPath, 123, nil
	}
	_ = db.Migrator().DropTable(&FileEditRequestPhoto{})
	_, err = svc.CreateEditRequest(EditRequestInput{
		FirstName: "A",
		LastName:  "B",
		FileID:    1,
		Filename:  "f",
		PhotosInApp: []PhotoInput{
			{Filename: "a.jpg", MimeType: "image/jpeg", DataBase64: "b64"},
		},
		IsEdited: false,
		RowID:    0,
	}, 1)
	if err == nil {
		t.Fatalf("expected photo insert err")
	}

	// Gallery upload error
	db = newTestDB(t)
	svc = &FileService{DB: db}
	call := 0
	uploadToGCSHook = func(_, _, _ string) (string, int64, error) {
		call++
		if call == 1 {
			return "gs://b/x", 1, nil // app photo ok
		}
		return "", 0, errors.New("gallery upload fail")
	}
	_, err = svc.CreateEditRequest(EditRequestInput{
		FirstName: "A",
		LastName:  "B",
		FileID:    1,
		Filename:  "f",
		PhotosInApp: []PhotoInput{
			{Filename: "a.jpg", MimeType: "image/jpeg", DataBase64: "b64"},
		},
		PhotosForGallery: []PhotoInput{
			{Filename: "b.jpg", MimeType: "image/jpeg", DataBase64: "b642"},
		},
		IsEdited: false,
		RowID:    0,
	}, 1)
	if err == nil {
		t.Fatalf("expected gallery upload err")
	}

	// documents: skip non-document, doc upload error
	db = newTestDB(t)
	svc = &FileService{DB: db}

	uploadToGCSHook = func(_, _, _ string) (string, int64, error) {
		return "", 0, errors.New("doc upload fail")
	}
	_, err = svc.CreateEditRequest(EditRequestInput{
		FirstName: "A", LastName: "B",
		FileID: 1, Filename: "f",
		IsEdited: false, RowID: 0,
		Documents: []DocumentInput{
			{DocumentType: "photos", Filename: "skip.png", DataBase64: "x"},
			{DocumentType: "document", Filename: "doc.pdf", DataBase64: "x", DocumentCategory: ""},
		},
	}, 1)
	if err == nil {
		t.Fatalf("expected doc upload err")
	}

	// full success (covers TempPrefix + RowPrefix logic + doc defaults)
	db = newTestDB(t)
	svc = &FileService{DB: db}

	uploadedPaths = nil
	uploadToGCSHook = func(base64, bucket, objectPath string) (string, int64, error) {
		uploadedPaths = append(uploadedPaths, objectPath)
		return "gs://" + bucket + "/" + objectPath, 123, nil
	}

	req, err := svc.CreateEditRequest(EditRequestInput{
		FirstName: "Athul", LastName: "N",
		FileID: 10, Filename: "master",
		IsEdited: false, RowID: 0,
		Consent: true,
		Changes: map[string][]EditChangeInput{
			"c": {{RowID: 0, FieldName: "Name", OldValue: "", NewValue: "X"}},
		},
		PhotosInApp: []PhotoInput{
			{Filename: "a.jpg", MimeType: "image/jpeg", DataBase64: "b64a", Comment: "hello"},
		},
		PhotosForGallery: []PhotoInput{
			{Filename: "g.jpg", MimeType: "image/jpeg", DataBase64: "b64g", Comment: "gallery"},
		},
		Documents: []DocumentInput{
			{DocumentType: "document", Filename: "id.pdf", DataBase64: "b64d", DocumentCategory: ""},
		},
	}, 55)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if req == nil || req.RequestID == 0 {
		t.Fatalf("expected request created")
	}

	// basePrefix must be TempPrefix because IsEdited=false OR RowID==0
	wantPrefix := util.TempPrefix(req.RequestID, "Athul", "N")
	for _, p := range uploadedPaths {
		if !strings.HasPrefix(p, wantPrefix+"/") {
			t.Fatalf("expected upload path to start with %q, got %q", wantPrefix+"/", p)
		}
	}

	// RowPrefix path (IsEdited=true and RowID!=0)
	uploadedPaths = nil
	_, err = svc.CreateEditRequest(EditRequestInput{
		FirstName: "A", LastName: "B",
		FileID: 10, Filename: "master",
		IsEdited: true, RowID: 123,
		PhotosInApp: []PhotoInput{
			{Filename: "a.jpg", MimeType: "image/jpeg", DataBase64: "b64"},
		},
	}, 55)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	wantRowPrefix := util.RowPrefix(123)
	if len(uploadedPaths) == 0 || !strings.HasPrefix(uploadedPaths[0], wantRowPrefix+"/") {
		t.Fatalf("expected rowprefix %q, got %#v", wantRowPrefix, uploadedPaths)
	}
}

func TestFileService_CreateEditRequest_PhotoExtension_FromFilenameOrMime(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	prevUpload := uploadToGCSHook
	t.Cleanup(func() { uploadToGCSHook = prevUpload })

	var uploadedPaths []string
	uploadToGCSHook = func(base64, bucket, objectPath string) (string, int64, error) {
		uploadedPaths = append(uploadedPaths, objectPath)
		return "gs://" + bucket + "/" + objectPath, 123, nil
	}

	req, err := svc.CreateEditRequest(EditRequestInput{
		FirstName: "A", LastName: "B",
		FileID: 1, Filename: "f",
		IsEdited: false, RowID: 0,
		PhotosInApp: []PhotoInput{
			// extension from filename
			{Filename: "x.png", MimeType: "image/png", DataBase64: "b64"},
			// no extension -> should come from mime
			{Filename: "noext", MimeType: "image/webp", DataBase64: "b64"},
			// neither helpful -> default .jpg
			{Filename: "unknown", MimeType: "application/octet-stream", DataBase64: "b64"},
		},
	}, 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if req == nil || req.RequestID == 0 {
		t.Fatalf("expected request created")
	}

	if len(uploadedPaths) != 3 {
		t.Fatalf("expected 3 uploads, got %d (%#v)", len(uploadedPaths), uploadedPaths)
	}
	if !strings.HasSuffix(uploadedPaths[0], ".png") {
		t.Fatalf("expected .png suffix, got %q", uploadedPaths[0])
	}
	if !strings.HasSuffix(uploadedPaths[1], ".webp") {
		t.Fatalf("expected .webp suffix, got %q", uploadedPaths[1])
	}
	if !strings.HasSuffix(uploadedPaths[2], ".jpg") {
		t.Fatalf("expected default .jpg suffix, got %q", uploadedPaths[2])
	}
}

func TestFileService_AchieverStoryRequestLifecycle(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	user := UserForTest{Email: "story@example.com", Role: "User", FirstName: "Story", LastName: "Submitter"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	master := File{Filename: "Shingwauk And Wawanosh Students Master List", Version: 3}
	if err := db.Create(&master).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}
	row := FileData{FileID: master.ID, Version: master.Version, RowData: datatypes.JSON([]byte(`{"Name":"Story person"}`))}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed file row: %v", err)
	}

	prevUpload := uploadToGCSHook
	prevGo := formSubmissionGoHook
	t.Cleanup(func() {
		uploadToGCSHook = prevUpload
		formSubmissionGoHook = prevGo
	})
	formSubmissionGoHook = func(func()) {}
	var uploadedData string
	var uploadedPath string
	uploadToGCSHook = func(data, bucket, objectPath string) (string, int64, error) {
		uploadedData = data
		uploadedPath = objectPath
		return "gs://" + bucket + "/" + objectPath, int64(len(data)), nil
	}

	request, err := svc.CreateAchieverStoryRequest(AchieverStoryRequestInput{
		FileID:    master.ID,
		RowID:     row.ID,
		FirstName: "Jane",
		LastName:  "Doe",
		StoryType: "text",
		StoryText: "A survivor's written achiever story.",
	}, user.ID)
	if err != nil {
		t.Fatalf("create text story request: %v", err)
	}
	if request.RequestType != fileEditRequestTypeAchieverStory || request.Status != fileEditRequestStatusPending {
		t.Fatalf("unexpected request: %#v", request)
	}

	var story AchieverStory
	if err := db.Where("request_id = ?", request.RequestID).First(&story).Error; err != nil {
		t.Fatalf("load pending story: %v", err)
	}
	if story.Status != fileEditRequestStatusPending || story.StoryText == "" ||
		story.ContentType != "application/pdf" || story.FileName != "Jane_Doe_story.pdf" || story.StoryURL == "" {
		t.Fatalf("unexpected pending story: %#v", story)
	}
	if !strings.HasPrefix(uploadedData, "data:application/pdf;base64,") || !strings.HasSuffix(uploadedPath, "Jane_Doe_story.pdf") {
		t.Fatalf("text story was not uploaded as PDF: data prefix=%q path=%q", uploadedData[:min(len(uploadedData), 32)], uploadedPath)
	}

	templateRequest, err := svc.CreateAchieverStoryRequest(AchieverStoryRequestInput{
		FileID:    master.ID,
		RowID:     row.ID,
		FirstName: "Thelma",
		LastName:  "Fair",
		StoryType: "text",
		Template: &AchieverStoryTemplateInput{
			DateOfBirth:              "1956-02-15",
			DateOfDeath:              "Not recorded. Would be 70 in 2026 if living.",
			Community:                "Shoal Lake #126",
			ResidentialSchoolHistory: "Attended Shingwauk Indian Residential School.",
			AchieversStory:           "Thelma served her community as a traditional healer.",
			Sources:                  []string{"https://example.org/source"},
		},
	}, user.ID)
	if err != nil {
		t.Fatalf("create structured story request: %v", err)
	}
	var templateStory AchieverStory
	if err := db.Where("request_id = ?", templateRequest.RequestID).First(&templateStory).Error; err != nil {
		t.Fatalf("load structured story: %v", err)
	}
	if templateStory.StoryText != "Thelma served her community as a traditional healer." ||
		templateStory.ContentType != "application/pdf" || templateStory.StoryURL == "" {
		t.Fatalf("unexpected structured story: %#v", templateStory)
	}
	if !strings.HasPrefix(uploadedData, "data:application/pdf;base64,") || !strings.HasSuffix(uploadedPath, "Thelma_Fair_story.pdf") {
		t.Fatalf("structured story was not uploaded as PDF: data prefix=%q path=%q", uploadedData[:min(len(uploadedData), 32)], uploadedPath)
	}

	if err := svc.ReviewAchieverStoryRequest(request.RequestID, "", nil, 99); err == nil {
		t.Fatal("expected a decision for the pending story")
	}
	if err := svc.ReviewAchieverStoryRequest(request.RequestID, "Approved for publication", []AchieverStoryReviewInput{{
		StoryID: story.ID,
		Status:  ReviewStatusApproved,
	}}, 99); err != nil {
		t.Fatalf("approve story request: %v", err)
	}

	published, err := svc.GetAchieverStoriesByRow(row.ID)
	if err != nil || len(published) != 1 || published[0].ID != story.ID {
		t.Fatalf("published stories = %#v, err=%v", published, err)
	}

	uploadToGCSHook = func(_, bucket, objectPath string) (string, int64, error) {
		uploadedPath = objectPath
		return "gs://" + bucket + "/" + objectPath, 12, nil
	}
	_, err = svc.CreateAchieverStoryRequest(AchieverStoryRequestInput{
		FileID:    master.ID,
		RowID:     row.ID,
		StoryType: "document",
		Document:  &DocumentInput{Filename: "story.docx", MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", DataBase64: "data:application/vnd.openxmlformats-officedocument.wordprocessingml.document;base64,ZA=="},
	}, user.ID)
	if err != nil {
		t.Fatalf("create document story request: %v", err)
	}
	if !strings.HasPrefix(uploadedPath, "achiever-stories/requests/") || !strings.HasSuffix(uploadedPath, "story.docx") {
		t.Fatalf("unexpected story upload path %q", uploadedPath)
	}

	videoRequest, err := svc.CreateAchieverStoryRequest(AchieverStoryRequestInput{
		FileID:    master.ID,
		RowID:     row.ID,
		StoryType: "video",
		Video: &DocumentInput{
			Filename:   "survivor-story.mp4",
			MimeType:   "video/mp4",
			Size:       1024,
			DataBase64: "data:video/mp4;base64,ZA==",
		},
	}, user.ID)
	if err != nil {
		t.Fatalf("create uploaded video story request: %v", err)
	}
	var uploadedVideo AchieverStory
	if err := db.Where("request_id = ?", videoRequest.RequestID).First(&uploadedVideo).Error; err != nil {
		t.Fatalf("load uploaded video story: %v", err)
	}
	if uploadedVideo.VideoURL != "" || uploadedVideo.StoryURL == "" || uploadedVideo.FileName != "survivor-story.mp4" || uploadedVideo.ContentType != "video/mp4" {
		t.Fatalf("unexpected uploaded video story: %#v", uploadedVideo)
	}
	if !strings.HasSuffix(uploadedPath, "survivor-story.mp4") {
		t.Fatalf("unexpected video upload path %q", uploadedPath)
	}

	linkedVideoURL := "https://www.youtube.com/watch?v=survivor-story"
	linkRequest, err := svc.CreateAchieverStoryRequest(AchieverStoryRequestInput{
		FileID:    master.ID,
		RowID:     row.ID,
		StoryType: "video",
		VideoURL:  linkedVideoURL,
	}, user.ID)
	if err != nil {
		t.Fatalf("create linked video story request: %v", err)
	}
	var linkedVideo AchieverStory
	if err := db.Where("request_id = ?", linkRequest.RequestID).First(&linkedVideo).Error; err != nil {
		t.Fatalf("load linked video story: %v", err)
	}
	if linkedVideo.VideoURL != linkedVideoURL || linkedVideo.StoryURL != "" || linkedVideo.FileName != "" {
		t.Fatalf("unexpected linked video story: %#v", linkedVideo)
	}

	_, err = svc.CreateAchieverStoryRequest(AchieverStoryRequestInput{
		FileID:    master.ID,
		RowID:     row.ID,
		StoryType: "document",
		Document:  &DocumentInput{Filename: "not-allowed.png", MimeType: "image/png", DataBase64: "data:image/png;base64,ZA=="},
	}, user.ID)
	if err == nil || !strings.Contains(err.Error(), "PDF, DOC, or DOCX") {
		t.Fatalf("expected invalid document error, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// GetEditRequests
// -----------------------------------------------------------------------------

func TestFileService_GetEditRequests_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	u := UserForTest{Email: "u@b.com", Role: "User", FirstName: "U", LastName: "L"}
	_ = db.Create(&u).Error

	// seed pending + completed
	r1 := FileEditRequest{UserID: uint(u.ID), Status: "pending", CreatedAt: time.Now(), FirstName: "E", LastName: "F", RowID: 0, IsEdited: true, Consent: true, ArchiveConsent: true, FileID: 1}
	r2 := FileEditRequest{UserID: uint(u.ID), Status: "completed", CreatedAt: time.Now().Add(-time.Hour), FirstName: "E2", LastName: "F2", RowID: 0, IsEdited: true, Consent: true, ArchiveConsent: false, FileID: 1, ReviewComment: "completed after review"}
	_ = db.Create(&r1).Error
	_ = db.Create(&r2).Error
	_ = db.Exec("UPDATE file_edit_request SET reviewer_comment = NULL WHERE request_id = ?", r1.RequestID).Error

	_ = db.Create(&FileEditRequestDetails{RequestID: r1.RequestID, FileID: 1, Filename: "x", RowID: 0, FieldName: "A", OldValue: "", NewValue: "1"}).Error
	_ = db.Create(&FileEditRequestDetails{RequestID: r2.RequestID, FileID: 1, Filename: "x", RowID: 0, FieldName: "A", OldValue: "", NewValue: "2"}).Error

	// default => pending only
	out, err := svc.GetEditRequests(nil, nil)
	if err != nil || len(out) != 1 || out[0].Status != "pending" {
		t.Fatalf("expected pending only, got %#v err=%v", out, err)
	}
	if out[0].ReviewComment != "" {
		t.Fatalf("expected empty review comment for NULL db value, got %#v", out[0].ReviewComment)
	}
	if !out[0].ArchiveConsent {
		t.Fatalf("expected archive consent to be returned")
	}

	// both filters => IN (...)
	statusCSV := "completed, pending"
	uid := uint(u.ID)
	out, err = svc.GetEditRequests(&statusCSV, &uid)
	if err != nil || len(out) != 2 {
		t.Fatalf("expected 2, got %d err=%v", len(out), err)
	}
	if out[0].Status != "pending" || out[0].ReviewComment != "" {
		t.Fatalf("expected pending request first with empty review comment, got %#v", out[0])
	}
	if out[1].Status != "completed" || out[1].ReviewComment != "completed after review" {
		t.Fatalf("expected completed request to include review comment, got %#v", out[1])
	}
	if out[1].ArchiveConsent {
		t.Fatalf("expected completed request archive consent to be false")
	}

	// garbage statusCSV => fallback pending
	garb := ",,,"
	out, err = svc.GetEditRequests(&garb, &uid)
	if err != nil || len(out) != 1 || out[0].Status != "pending" {
		t.Fatalf("expected pending fallback, got %#v err=%v", out, err)
	}

	invalidStatus := "approved"
	if _, err = svc.GetEditRequests(&invalidStatus, &uid); err == nil || err.Error() != "status must be pending or completed" {
		t.Fatalf("expected invalid parent status error, got %v", err)
	}

	// scan error
	breakDB(t, db)
	_, err = svc.GetEditRequests(nil, nil)
	if err == nil {
		t.Fatalf("expected scan err")
	}

	// details query error
	db = newTestDB(t)
	svc = &FileService{DB: db}
	_ = db.Create(&u).Error
	r3 := FileEditRequest{UserID: uint(u.ID), Status: "pending", CreatedAt: time.Now(), FirstName: "E", LastName: "F", RowID: 0, IsEdited: true, Consent: true, FileID: 1}
	_ = db.Create(&r3).Error
	_ = db.Migrator().DropTable(&FileEditRequestDetails{})
	_, err = svc.GetEditRequests(nil, nil)
	if err == nil {
		t.Fatalf("expected details query err")
	}
}

// -----------------------------------------------------------------------------
// ReviewEditRequest (edited + not edited)
// -----------------------------------------------------------------------------

func TestFileService_ReviewEditRequest_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}

	// restore hooks
	prevMove := moveGCSFolderHook
	t.Cleanup(func() { moveGCSFolderHook = prevMove })
	moveGCSFolderHook = func(bucket, src, dst string) (map[string]string, error) {
		return map[string]string{}, nil
	}

	if err := svc.ReviewEditRequest(1, "approved", "Test comment", nil, 9); err == nil || err.Error() != "status must be completed" {
		t.Fatalf("expected approved parent status to be rejected, got %v", err)
	}
	if err := svc.ReviewEditRequest(1, "rejected", "Test comment", nil, 9); err == nil || err.Error() != "status must be completed" {
		t.Fatalf("expected rejected parent status to be rejected, got %v", err)
	}

	// 0) update new_value error
	_ = db.Migrator().DropTable(&FileEditRequestDetails{})
	if err := svc.ReviewEditRequest(1, "completed", "Test comment", []FileEditRequestDetails{{ID: 1, NewValue: "x"}}, 9); err == nil {
		t.Fatalf("expected update new_value err")
	}

	// recreate
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}

	// request not found
	if err := svc.ReviewEditRequest(999, "completed", "Test comment", nil, 9); err == nil {
		t.Fatalf("expected request not found err")
	}

	// allDetails fetch error
	req := FileEditRequest{UserID: 1, Status: "pending", IsEdited: true, FirstName: "A", LastName: "B", FileID: 1}
	_ = db.Create(&req).Error
	_ = db.Migrator().DropTable(&FileEditRequestDetails{})
	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", nil, 9); err == nil {
		t.Fatalf("expected details fetch err")
	}

	// --- NOT EDITED path errors + success ---
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}

	req = FileEditRequest{UserID: 1, Status: "pending", IsEdited: false, FirstName: "A", LastName: "B", RowID: 0, FileID: 10}
	_ = db.Create(&req).Error
	d1 := FileEditRequestDetails{RequestID: req.RequestID, FileID: 10, Filename: "f", RowID: 0, FieldName: "Name", OldValue: "", NewValue: "X"}
	_ = db.Create(&d1).Error

	// insert file_data error
	_ = db.Migrator().DropTable(&FileData{})
	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", []FileEditRequestDetails{{ID: d1.ID, NewValue: "Y"}}, 9); err == nil {
		t.Fatalf("expected insert file_data err")
	}

	// move folder hook error
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}
	moveGCSFolderHook = func(bucket, src, dst string) (map[string]string, error) {
		return nil, errors.New("move fail")
	}
	req = FileEditRequest{UserID: 1, Status: "pending", IsEdited: false, FirstName: "A", LastName: "B", RowID: 0, FileID: 10}
	_ = db.Create(&req).Error
	_ = db.Create(&FileEditRequestDetails{RequestID: req.RequestID, FileID: 10, Filename: "f", RowID: 0, FieldName: "Name", OldValue: "", NewValue: "X"}).Error
	_ = db.Create(&FileEditRequestPhoto{RequestID: req.RequestID, RowID: 0, FileID: 10, PhotoURL: "gs://nordik-drive-photos/requests/x.jpg", FileName: "x.jpg", DocumentType: "photos"}).Error
	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", nil, 9); err == nil {
		t.Fatalf("expected move folder err")
	}

	// NOT EDITED success
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}
	moveGCSFolderHook = func(bucket, src, dst string) (map[string]string, error) { return map[string]string{}, nil }

	req = FileEditRequest{UserID: 1, Status: "pending", IsEdited: false, FirstName: "A", LastName: "B", RowID: 0, FileID: 10}
	_ = db.Create(&req).Error
	d1 = FileEditRequestDetails{RequestID: req.RequestID, FileID: 10, Filename: "f", RowID: 0, FieldName: "Name", OldValue: "", NewValue: "X"}
	_ = db.Create(&d1).Error

	photo := FileEditRequestPhoto{
		RequestID:    req.RequestID,
		RowID:        0,
		FileID:       10,
		FileName:     "x.jpg",
		PhotoURL:     "gs://nordik-drive-photos/" + util.TempPrefix(req.RequestID, "A", "B") + "/x.jpg",
		DocumentType: "photos",
	}
	_ = db.Create(&photo).Error

	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", []FileEditRequestDetails{{ID: d1.ID, NewValue: "Y"}}, 9); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var reqUpdated FileEditRequest
	_ = db.Where("request_id = ?", req.RequestID).First(&reqUpdated).Error
	if reqUpdated.Status != "completed" || reqUpdated.ReviewedBy == nil || *reqUpdated.ReviewedBy != 9 || reqUpdated.RowID == 0 {
		t.Fatalf("unexpected updated request: %#v", reqUpdated)
	}
	var detailUpdated FileEditRequestDetails
	_ = db.Where("id = ?", d1.ID).First(&detailUpdated).Error
	if detailUpdated.Status != "approved" || detailUpdated.ReviewComment != "Test comment" {
		t.Fatalf("unexpected updated detail review fields: %#v", detailUpdated)
	}

	// --- EDITED path errors + success ---
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}

	req = FileEditRequest{UserID: 1, Status: "pending", IsEdited: true, RowID: 123, FileID: 10}
	_ = db.Create(&req).Error

	// file_data not found
	_ = db.Create(&FileEditRequestDetails{RequestID: req.RequestID, FileID: 10, RowID: 123, FieldName: "A", NewValue: "X"}).Error
	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", nil, 9); err == nil {
		t.Fatalf("expected file_data not found")
	}

	// row_data unmarshal error
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}
	req = FileEditRequest{UserID: 1, Status: "pending", IsEdited: true, RowID: 123, FileID: 10}
	_ = db.Create(&req).Error
	_ = db.Create(&FileEditRequestDetails{RequestID: req.RequestID, FileID: 10, RowID: 123, FieldName: "A", NewValue: "X"}).Error
	_ = db.Create(&FileData{ID: 123, FileID: 10, RowData: datatypes.JSON([]byte(`{bad`)), Version: 1}).Error
	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", nil, 9); err == nil {
		t.Fatalf("expected row_data parse error")
	}

	// update file_data error
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}
	req = FileEditRequest{UserID: 1, Status: "pending", IsEdited: true, RowID: 123, FileID: 10}
	_ = db.Create(&req).Error
	_ = db.Create(&FileEditRequestDetails{RequestID: req.RequestID, FileID: 10, RowID: 123, FieldName: "A", NewValue: "X"}).Error
	_ = db.Create(&FileData{ID: 123, FileID: 10, RowData: datatypes.JSON([]byte(`{"A":"old"}`)), Version: 1}).Error
	_ = db.Migrator().DropTable(&FileData{})
	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", nil, 9); err == nil {
		t.Fatalf("expected update file_data err")
	}

	// status update error (drop request table before final update)
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}
	req = FileEditRequest{UserID: 1, Status: "pending", IsEdited: true, RowID: 123, FileID: 10}
	_ = db.Create(&req).Error
	_ = db.Create(&FileEditRequestDetails{RequestID: req.RequestID, FileID: 10, RowID: 123, FieldName: "A", NewValue: "X"}).Error
	_ = db.Create(&FileData{ID: 123, FileID: 10, RowData: datatypes.JSON([]byte(`{"A":"old"}`)), Version: 1}).Error
	_ = db.Migrator().DropTable(&FileEditRequest{})
	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", nil, 9); err == nil {
		t.Fatalf("expected status update err")
	}

	// EDITED success
	db = newTestDB(t)
	svc = &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}
	req = FileEditRequest{UserID: 1, Status: "pending", IsEdited: true, RowID: 123, FileID: 10}
	_ = db.Create(&req).Error
	_ = db.Create(&FileEditRequestDetails{RequestID: req.RequestID, FileID: 10, RowID: 123, FieldName: "A", NewValue: "X"}).Error
	_ = db.Create(&FileData{ID: 123, FileID: 10, RowData: datatypes.JSON([]byte(`{"A":"old","B":"keep"}`)), Version: 1}).Error
	if err := svc.ReviewEditRequest(req.RequestID, "completed", "Test comment", nil, 9); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var updatedReq FileEditRequest
	_ = db.Where("request_id = ?", req.RequestID).First(&updatedReq).Error
	if updatedReq.Status != "completed" || updatedReq.ReviewedBy == nil || *updatedReq.ReviewedBy != 9 {
		t.Fatalf("unexpected req: %#v", updatedReq)
	}
	var updatedDetail FileEditRequestDetails
	_ = db.Where("request_id = ?", req.RequestID).First(&updatedDetail).Error
	if updatedDetail.Status != "approved" || updatedDetail.ReviewComment != "Test comment" {
		t.Fatalf("unexpected detail review fields: %#v", updatedDetail)
	}
}

func TestFileService_ReviewEditRequest_UpdatesOnlyApprovedDetails(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}

	req := FileEditRequest{UserID: 1, Status: "pending", IsEdited: true, RowID: 123, FileID: 10}
	if err := db.Create(&req).Error; err != nil {
		t.Fatalf("seed request: %v", err)
	}

	approvedDetail := FileEditRequestDetails{
		RequestID: req.RequestID,
		FileID:    10,
		Filename:  "f",
		RowID:     123,
		FieldName: "A",
		OldValue:  "old-a",
		NewValue:  "new-a",
	}
	rejectedDetail := FileEditRequestDetails{
		RequestID: req.RequestID,
		FileID:    10,
		Filename:  "f",
		RowID:     123,
		FieldName: "B",
		OldValue:  "old-b",
		NewValue:  "new-b",
	}
	if err := db.Create(&approvedDetail).Error; err != nil {
		t.Fatalf("seed approved detail: %v", err)
	}
	if err := db.Create(&rejectedDetail).Error; err != nil {
		t.Fatalf("seed rejected detail: %v", err)
	}
	if err := db.Create(&FileData{
		ID:      123,
		FileID:  10,
		RowData: datatypes.JSON([]byte(`{"A":"old-a","B":"old-b"}`)),
		Version: 1,
	}).Error; err != nil {
		t.Fatalf("seed file data: %v", err)
	}

	err := svc.ReviewEditRequest(req.RequestID, "completed", "request reviewed", []FileEditRequestDetails{
		{ID: approvedDetail.ID, NewValue: "approved-a", Status: "approved", ReviewComment: "ok"},
		{ID: rejectedDetail.ID, NewValue: "rejected-b", Status: "rejected", ReviewComment: "do not use"},
	}, 9)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var fileData FileData
	if err := db.Where("id = ?", 123).First(&fileData).Error; err != nil {
		t.Fatalf("load file data: %v", err)
	}

	var row map[string]string
	if err := json.Unmarshal(fileData.RowData, &row); err != nil {
		t.Fatalf("unmarshal row_data: %v", err)
	}
	if row["A"] != "approved-a" {
		t.Fatalf("expected approved field to update, got row=%#v", row)
	}
	if row["B"] != "old-b" {
		t.Fatalf("expected rejected field to stay unchanged, got row=%#v", row)
	}

	var details []FileEditRequestDetails
	if err := db.Where("request_id = ?", req.RequestID).Order("id ASC").Find(&details).Error; err != nil {
		t.Fatalf("load details: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("expected 2 details, got %#v", details)
	}
	if details[0].Status != "approved" || details[0].ReviewComment != "ok" {
		t.Fatalf("unexpected approved detail review fields: %#v", details[0])
	}
	if details[1].Status != "rejected" || details[1].ReviewComment != "do not use" {
		t.Fatalf("unexpected rejected detail review fields: %#v", details[1])
	}
}

func TestFileService_ReviewEditRequest_AllRejectedDetailsCompletesWithoutFileUpdate(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db, Mailer: mailer.NewService("test@example.com", "key", "domain", "http://localhost")}

	req := FileEditRequest{UserID: 1, Status: "pending", IsEdited: true, RowID: 123, FileID: 10}
	if err := db.Create(&req).Error; err != nil {
		t.Fatalf("seed request: %v", err)
	}

	detail := FileEditRequestDetails{
		RequestID: req.RequestID,
		FileID:    10,
		Filename:  "f",
		RowID:     123,
		FieldName: "A",
		OldValue:  "old-a",
		NewValue:  "new-a",
	}
	if err := db.Create(&detail).Error; err != nil {
		t.Fatalf("seed detail: %v", err)
	}
	if err := db.Create(&FileData{
		ID:      123,
		FileID:  10,
		RowData: datatypes.JSON([]byte(`{"A":"old-a"}`)),
		Version: 1,
	}).Error; err != nil {
		t.Fatalf("seed file data: %v", err)
	}

	err := svc.ReviewEditRequest(req.RequestID, "completed", "review complete", []FileEditRequestDetails{
		{ID: detail.ID, NewValue: "rejected-a", Status: "rejected", ReviewComment: "not supported"},
	}, 9)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var updatedReq FileEditRequest
	if err := db.Where("request_id = ?", req.RequestID).First(&updatedReq).Error; err != nil {
		t.Fatalf("load request: %v", err)
	}
	if updatedReq.Status != "completed" {
		t.Fatalf("expected request completed, got %#v", updatedReq)
	}

	var fileData FileData
	if err := db.Where("id = ?", 123).First(&fileData).Error; err != nil {
		t.Fatalf("load file data: %v", err)
	}

	var row map[string]string
	if err := json.Unmarshal(fileData.RowData, &row); err != nil {
		t.Fatalf("unmarshal row_data: %v", err)
	}
	if row["A"] != "old-a" {
		t.Fatalf("expected rejected field to stay unchanged, got row=%#v", row)
	}
}

// -----------------------------------------------------------------------------
// Photos/docs queries + ReviewPhotos
// -----------------------------------------------------------------------------

func TestFileService_PhotoDocQueries_And_ReviewPhotos(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	// seed photos/docs
	p1 := FileEditRequestPhoto{
		RequestID:    1,
		RowID:        10,
		DocumentType: "photos",
		Status:       "pending",
		FileName:     "p.jpg",
	}
	d1 := FileEditRequestPhoto{
		RequestID:    1,
		RowID:        10,
		DocumentType: "document",
		Status:       "pending",
		FileName:     "d.pdf",
	}
	_ = db.Create(&p1).Error
	_ = db.Create(&d1).Error

	photos, err := svc.GetPhotosByRequest(1)
	if err != nil || len(photos) != 1 {
		t.Fatalf("expected 1 photo, got %d err=%v", len(photos), err)
	}
	docs, err := svc.GetDocsByRequest(1)
	if err != nil || len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d err=%v", len(docs), err)
	}

	// approved-by-row queries
	now := time.Now()
	_ = db.Model(&FileEditRequestPhoto{}).Where("id = ?", p1.ID).Updates(map[string]any{
		"status":      "approved",
		"reviewed_at": now,
	}).Error
	_ = db.Model(&FileEditRequestPhoto{}).Where("id = ?", d1.ID).Updates(map[string]any{
		"status":      "approved",
		"reviewed_at": now,
	}).Error

	phByRow, err := svc.GetPhotosByRow(uint(10))
	if err != nil || len(phByRow) != 1 {
		t.Fatalf("expected 1 approved photo by row, got %d err=%v", len(phByRow), err)
	}
	docByRow, err := svc.GetDocsByRow(uint(10))
	if err != nil || len(docByRow) != 1 {
		t.Fatalf("expected 1 approved doc by row, got %d err=%v", len(docByRow), err)
	}

	stories := []AchieverStory{
		{FileID: 1, RowID: 10, StoryType: "document", Status: "approved", FileName: "approved.pdf"},
		{FileID: 1, RowID: 10, StoryType: "text", Status: "pending", StoryText: "not yet published"},
		{FileID: 1, RowID: 11, StoryType: "video", Status: "approved", VideoURL: "https://example.com/story"},
	}
	if err := db.Create(&stories).Error; err != nil {
		t.Fatalf("seed achiever stories: %v", err)
	}
	approvedStories, err := svc.GetAchieverStoriesByRow(10)
	if err != nil || len(approvedStories) != 1 || approvedStories[0].FileName != "approved.pdf" {
		t.Fatalf("expected one approved achiever story, got %#v err=%v", approvedStories, err)
	}

	// ReviewPhotos: empty reviews should error
	if err := svc.ReviewPhotos(nil, 99); err == nil {
		t.Fatalf("expected empty review error")
	}

	// ReviewPhotos: invalid photo id
	if err := svc.ReviewPhotos([]PhotoReviewInput{
		{
			PhotoID:       0,
			Status:        "approved",
			ReviewComment: "ok",
		},
	}, 99); err == nil {
		t.Fatalf("expected photo_id validation error")
	}

	// ReviewPhotos: invalid status
	if err := svc.ReviewPhotos([]PhotoReviewInput{
		{
			PhotoID:       p1.ID,
			Status:        "pending",
			ReviewComment: "not allowed here",
		},
	}, 99); err == nil {
		t.Fatalf("expected invalid status error")
	}

	// ReviewPhotos approve/update error
	_ = db.Migrator().DropTable(&FileEditRequestPhoto{})
	if err := svc.ReviewPhotos([]PhotoReviewInput{
		{
			PhotoID:       1,
			Status:        "approved",
			ReviewComment: "good",
		},
	}, 99); err == nil {
		t.Fatalf("expected review update error")
	}

	// ReviewPhotos success (approve + reject)
	db = newTestDB(t)
	svc = &FileService{DB: db}

	a := FileEditRequestPhoto{
		RequestID:    1,
		RowID:        10,
		DocumentType: "photos",
		Status:       "rejected",
		FileName:     "a.jpg",
	}
	r := FileEditRequestPhoto{
		RequestID:    1,
		RowID:        10,
		DocumentType: "photos",
		Status:       "approved",
		FileName:     "r.jpg",
	}
	_ = db.Create(&a).Error
	_ = db.Create(&r).Error

	if err := svc.ReviewPhotos([]PhotoReviewInput{
		{
			PhotoID:       a.ID,
			Status:        "approved",
			ReviewComment: "looks good",
		},
		{
			PhotoID:       r.ID,
			Status:        "rejected",
			ReviewComment: "blurred image",
		},
	}, 99); err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	var aa, rr FileEditRequestPhoto
	_ = db.First(&aa, a.ID).Error
	_ = db.First(&rr, r.ID).Error

	if aa.Status != "approved" || rr.Status != "rejected" {
		t.Fatalf("unexpected review result: approved=%v rejected=%v", aa.Status, rr.Status)
	}

	if aa.ReviewComment != "looks good" {
		t.Fatalf("expected approve comment saved, got %q", aa.ReviewComment)
	}
	if rr.ReviewComment != "blurred image" {
		t.Fatalf("expected reject comment saved, got %q", rr.ReviewComment)
	}

	if aa.ReviewedAt == nil || rr.ReviewedAt == nil {
		t.Fatalf("expected reviewed_at to be set")
	}

	if aa.ReviewBy == nil || *aa.ReviewBy != 99 {
		t.Fatalf("expected approve reviewer id 99, got %#v", aa.ReviewBy)
	}
	if rr.ReviewBy == nil || *rr.ReviewBy != 99 {
		t.Fatalf("expected reject reviewer id 99, got %#v", rr.ReviewBy)
	}
}

// -----------------------------------------------------------------------------
// GCS functions + ioReadAll full branch coverage
// -----------------------------------------------------------------------------

func TestIOReadAll_AllBranches_WithoutNetwork(t *testing.T) {
	// EOF branch (err.Error()=="EOF")
	{
		sr := &storage.Reader{}
		inner := &errReadCloser{
			readFn: func(p []byte) (int, error) { return 0, errors.New("EOF") },
		}
		forceStorageReaderInner(t, sr, inner)
		data, err := ioReadAll(sr)
		if err != nil || string(data) != "" {
			t.Fatalf("expected empty,nil got=%q err=%v", string(data), err)
		}
	}

	// context.Canceled branch
	{
		sr := &storage.Reader{}
		inner := &errReadCloser{
			readFn: func(p []byte) (int, error) { return 0, context.Canceled },
		}
		forceStorageReaderInner(t, sr, inner)
		_, err := ioReadAll(sr)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	}

	// context.DeadlineExceeded branch
	{
		sr := &storage.Reader{}
		inner := &errReadCloser{
			readFn: func(p []byte) (int, error) { return 0, context.DeadlineExceeded },
		}
		forceStorageReaderInner(t, sr, inner)
		_, err := ioReadAll(sr)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected deadline exceeded, got %v", err)
		}
	}

	// contains "eof" branch (e.g., "unexpected EOF")
	{
		sr := &storage.Reader{}
		inner := &errReadCloser{
			readFn: func(p []byte) (int, error) { return 0, errors.New("unexpected EOF") },
		}
		forceStorageReaderInner(t, sr, inner)
		data, err := ioReadAll(sr)
		if err != nil || string(data) != "" {
			t.Fatalf("expected empty,nil got=%q err=%v", string(data), err)
		}
	}

	// generic error branch
	{
		sr := &storage.Reader{}
		inner := &errReadCloser{
			readFn: func(p []byte) (int, error) { return 0, errors.New("boom") },
		}
		forceStorageReaderInner(t, sr, inner)
		_, err := ioReadAll(sr)
		if err == nil || err.Error() != "boom" {
			t.Fatalf("expected boom, got %v", err)
		}
	}

	// normal read path (n>0 then EOF)
	{
		sr := &storage.Reader{}
		step := 0
		inner := &errReadCloser{
			readFn: func(p []byte) (int, error) {
				step++
				if step == 1 {
					copy(p, []byte("abc"))
					return 3, nil
				}
				return 0, errors.New("EOF")
			},
		}
		forceStorageReaderInner(t, sr, inner)
		data, err := ioReadAll(sr)
		if err != nil || string(data) != "abc" {
			t.Fatalf("expected abc,nil got=%q err=%v", string(data), err)
		}
	}
}

func TestFileService_GCS_Readers_AllBranches(t *testing.T) {
	db := newTestDB(t)
	svc := &FileService{DB: db}

	srv, bucket := withFakeGCS(t)

	// GetDocBytes: non-document guard
	doc := FileEditRequestPhoto{DocumentType: "photos"}
	_ = db.Create(&doc).Error
	if _, _, _, err := svc.GetDocBytes(doc.ID); err == nil {
		t.Fatalf("expected non-document err")
	}

	// GetDocBytes: BUCKET_NAME missing
	_ = os.Unsetenv("BUCKET_NAME")
	db2 := newTestDB(t)
	svc2 := &FileService{DB: db2}
	doc2 := FileEditRequestPhoto{DocumentType: "document", PhotoURL: "gs://x/y.pdf", FileName: ""}
	_ = db2.Create(&doc2).Error
	if _, _, _, err := svc2.GetDocBytes(doc2.ID); err == nil {
		t.Fatalf("expected BUCKET_NAME env err")
	}

	// Success GetDocBytes (also covers filename fallback)
	_ = os.Setenv("BUCKET_NAME", bucket)

	object := "docs/test.pdf"
	content := []byte("%PDF-1.4 test")
	putGCSObject(t, srv, bucket, object, content, "") // empty ct => service falls back to DetectContentType

	doc3 := FileEditRequestPhoto{
		DocumentType: "document",
		PhotoURL:     "gs://" + bucket + "/" + object,
		FileName:     "", // triggers path.Base(objectPath) fallback
	}
	_ = db.Create(&doc3).Error

	data, ct, filename, err := svc.GetDocBytes(doc3.ID)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if string(data) != string(content) {
		t.Fatalf("bytes mismatch")
	}
	if ct == "" {
		t.Fatalf("expected contentType derived")
	}
	if filename != path.Base(object) {
		t.Fatalf("expected filename %q got %q", path.Base(object), filename)
	}

	// GetPhotoBytes success
	photoObj := "photos/p1.jpg"
	photoBytes := []byte{0xFF, 0xD8, 0xFF, 0x00}
	putGCSObject(t, srv, bucket, photoObj, photoBytes, "image/jpeg")

	p := FileEditRequestPhoto{PhotoURL: "gs://" + bucket + "/" + photoObj, DocumentType: "photos"}
	_ = db.Create(&p).Error
	got, ct2, err := svc.GetPhotoBytes(p.ID)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != len(photoBytes) || ct2 != "image/jpeg" {
		t.Fatalf("unexpected photo bytes/ct: len=%d ct=%q", len(got), ct2)
	}

	// GetPhotoBytes error: object missing
	pMissing := FileEditRequestPhoto{PhotoURL: "gs://" + bucket + "/photos/missing.jpg", DocumentType: "photos"}
	_ = db.Create(&pMissing).Error
	if _, _, err := svc.GetPhotoBytes(pMissing.ID); err == nil {
		t.Fatalf("expected missing object error")
	}

	// OpenMediaHandle: kind guard + invalid url + bucket-from-env missing + success + disposition logic
	db3 := newTestDB(t)
	svc3 := &FileService{DB: db3}

	// kind mismatch
	rec := FileEditRequestPhoto{DocumentType: "document", PhotoURL: "gs://x/y.pdf"}
	_ = db3.Create(&rec).Error
	if _, _, _, _, err := svc3.OpenMediaHandle(context.Background(), rec.ID, "photo"); err == nil {
		t.Fatalf("expected kind mismatch error")
	}

	// invalid gs url format
	rec2 := FileEditRequestPhoto{DocumentType: "photos", PhotoURL: "http://x/y"}
	_ = db3.Create(&rec2).Error
	if _, _, _, _, err := svc3.OpenMediaHandle(context.Background(), rec2.ID, ""); err == nil {
		t.Fatalf("expected invalid gs url error")
	}

	// bucket empty in URL and BUCKET_NAME empty => error
	_ = os.Unsetenv("BUCKET_NAME")
	rec3 := FileEditRequestPhoto{DocumentType: "photos", PhotoURL: "gs:///obj.txt", FileName: ""}
	_ = db3.Create(&rec3).Error
	if _, _, _, _, err := svc3.OpenMediaHandle(context.Background(), rec3.ID, ""); err == nil {
		t.Fatalf("expected bucket missing error")
	}

	// restore bucket env for success
	_ = os.Setenv("BUCKET_NAME", bucket)

	// pdf => inline
	pdfObj := "media/a.pdf"
	putGCSObject(t, srv, bucket, pdfObj, []byte("%PDF-x"), "application/pdf")
	rec4 := FileEditRequestPhoto{DocumentType: "document", PhotoURL: "gs://" + bucket + "/" + pdfObj, FileName: ""}
	_ = db3.Create(&rec4).Error

	rc, fn, ct3, disp, err := svc3.OpenMediaHandle(context.Background(), rec4.ID, "document")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if fn != path.Base(pdfObj) || ct3 != "application/pdf" || disp != "inline" {
		t.Fatalf("unexpected: fn=%q ct=%q disp=%q", fn, ct3, disp)
	}
	_ = rc.Close()

	// non-image + non-pdf => attachment
	txtObj := "media/a.txt"
	putGCSObject(t, srv, bucket, txtObj, []byte("hello"), "text/plain")
	rec5 := FileEditRequestPhoto{DocumentType: "photos", PhotoURL: "gs://" + bucket + "/" + txtObj, FileName: "named.txt"}
	_ = db3.Create(&rec5).Error

	rc2, fn2, ct4, disp2, err := svc3.OpenMediaHandle(context.Background(), rec5.ID, "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if fn2 != "named.txt" || ct4 != "text/plain" || disp2 != "attachment" {
		t.Fatalf("unexpected: fn=%q ct=%q disp=%q", fn2, ct4, disp2)
	}
	_ = rc2.Close()

	// readFromGCS: invalid url + bucket missing + success
	_, _, _, err = svc.readFromGCS("http://x", "")
	if err == nil {
		t.Fatalf("expected invalid gs url")
	}
	_ = os.Unsetenv("BUCKET_NAME")
	_, _, _, err = svc.readFromGCS("gs:///obj.txt", "")
	if err == nil {
		t.Fatalf("expected bucket missing")
	}
	_ = os.Setenv("BUCKET_NAME", bucket)

	obj := "bytes/x.bin"
	putGCSObject(t, srv, bucket, obj, []byte("xyz"), "")
	bts, ctt, fname, err := svc.readFromGCS("gs://"+bucket+"/"+obj, "")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if string(bts) != "xyz" || ctt == "" || fname != path.Base(obj) {
		t.Fatalf("unexpected readFromGCS: b=%q ct=%q fn=%q", string(bts), ctt, fname)
	}

	// gcsReadHandle Close/Read: cover both methods using a forced storage.Reader
	h := &gcsReadHandle{
		Client: nil,
		Reader: func() *storage.Reader {
			sr := &storage.Reader{}
			step := 0
			forceStorageReaderInner(t, sr, &errReadCloser{
				readFn: func(p []byte) (int, error) {
					step++
					if step == 1 {
						copy(p, []byte("hi"))
						return 2, nil
					}
					return 0, errors.New("EOF")
				},
				closeFn: func() error { return nil },
			})
			return sr
		}(),
	}

	buf := make([]byte, 8)
	n, rerr := h.Read(buf)
	if n != 2 || string(buf[:2]) != "hi" || rerr != nil {
		t.Fatalf("unexpected read: n=%d data=%q err=%v", n, string(buf[:2]), rerr)
	}
	_ = h.Close()
}
