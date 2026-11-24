package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newMockDBChatSvc(t *testing.T) (*gorm.DB, sqlmock.Sqlmock, func()) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}

	gdb, err := gorm.Open(postgres.New(postgres.Config{
		Conn:                 sqlDB,
		PreferSimpleProtocol: true,
	}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}

	return gdb, mock, func() { _ = sqlDB.Close() }
}

func makeAudioFileHeader(t *testing.T, contentType string, data []byte) *multipart.FileHeader {
	t.Helper()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)

	partHeader := make(textprotoMIMEHeader)
	partHeader.Set("Content-Disposition", `form-data; name="audio"; filename="a.bin"`)
	if contentType != "" {
		partHeader.Set("Content-Type", contentType)
	}
	part, err := w.CreatePart(partHeader.toHeader())
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	_, _ = part.Write(data)
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/x", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if err := req.ParseMultipartForm(10 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	return req.MultipartForm.File["audio"][0]
}

type textprotoMIMEHeader map[string][]string

func (h textprotoMIMEHeader) Set(k, v string) { h[k] = []string{v} }

func (h textprotoMIMEHeader) toHeader() map[string][]string {
	out := map[string][]string{}
	for k, v := range h {
		out[k] = v
	}
	return out
}

func expectChatFileLookup(mock sqlmock.Sqlmock, filename string, descriptions ...string) {
	description := ""
	if len(descriptions) > 0 {
		description = descriptions[0]
	}
	mock.ExpectQuery(`(?i)select.*id.*version.*description.*from.*file.*where.*filename.*order.*version.*desc.*limit`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "version", "description"}).
			AddRow(uint(1), 1, description))
}

func expectChatRowsLookup(mock sqlmock.Sqlmock, rows ...string) {
	result := sqlmock.NewRows([]string{"id", "row_data"})
	for idx, row := range rows {
		result.AddRow(uint(idx+1), row)
	}
	mock.ExpectQuery(`(?i)select.*id.*row_data.*from.*file_data.*where.*file_id.*version.*order.*id.*asc`).
		WillReturnRows(result)
}

func expectChatNormalizedRowsLookup(mock sqlmock.Sqlmock, rows ...string) {
	result := sqlmock.NewRows([]string{
		"source_row_id",
		"canonical_name",
		"canonical_community",
		"canonical_school",
		"search_text",
		"row_data_normalized",
	})
	for idx, row := range rows {
		canonicalName, canonicalCommunity, canonicalSchool, searchText := normalizedChatRowColumns(row)
		result.AddRow(uint(idx+1), canonicalName, canonicalCommunity, canonicalSchool, searchText, row)
	}
	mock.ExpectQuery(`(?i)select.*source_row_id.*canonical_name.*canonical_community.*canonical_school.*search_text.*row_data_normalized.*from.*file_data_normalized.*where.*file_id.*version.*status.*normalization_version.*order.*source_row_id.*asc`).
		WillReturnRows(result)
}

func expectDatabaseDimensionValues(mock sqlmock.Sqlmock, field string, values ...databaseDimensionValue) {
	column := "canonical_" + field
	result := sqlmock.NewRows([]string{"normalized", "display", "record_count"})
	for _, value := range values {
		result.AddRow(value.Normalized, value.Display, value.Count)
	}
	mock.ExpectQuery(`(?i)select.*` + column + `.*record_count.*from.*file_data_normalized.*group by.*` + column).
		WillReturnRows(result)
}

func expectDatabaseCount(mock sqlmock.Sqlmock, count int64) {
	mock.ExpectQuery(`(?i)select.*count.*from.*file_data_normalized`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(count))
}

func expectDatabaseNamedRecords(mock sqlmock.Sqlmock, records ...databaseNamedRecord) {
	result := sqlmock.NewRows([]string{"source_row_id", "display_name"})
	for _, record := range records {
		result.AddRow(record.SourceRowID, record.DisplayName)
	}
	mock.ExpectQuery(`(?i)select.*source_row_id.*display_name.*from.*file_data_normalized.*order by.*canonical_name.*source_row_id`).
		WillReturnRows(result)
}

func expectDatabaseConfiguredValues(mock sqlmock.Sqlmock, values ...databaseConfiguredValue) {
	result := sqlmock.NewRows([]string{"display_value", "record_count"})
	for _, value := range values {
		result.AddRow(value.Display, value.Count)
	}
	mock.ExpectQuery(`(?i)select.*display_value.*count.*from.*jsonb_extract_path_text.*file_data_normalized`).
		WillReturnRows(result)
}

func normalizedChatRowColumns(row string) (string, string, string, string) {
	var payload map[string]any
	_ = json.Unmarshal([]byte(row), &payload)

	var canonicalName string
	var canonicalCommunity string
	var canonicalSchool string
	var searchValues []string

	if canonical, ok := payload["canonical"].(map[string]any); ok {
		canonicalName, _ = canonical["display_name"].(string)
		canonicalCommunity, _ = canonical["community"].(string)
		canonicalSchool, _ = canonical["school"].(string)
		searchValues = append(searchValues, flattenStringValues(canonical)...)
	}
	if chatPayload, ok := payload["chat"].(map[string]any); ok {
		if defaultBundle, ok := chatPayload["default_bundle"].(map[string]any); ok {
			searchValues = append(searchValues, flattenStringValues(defaultBundle)...)
		}
		if narrativeBundle, ok := chatPayload["narrative_bundle"].(map[string]any); ok {
			searchValues = append(searchValues, flattenStringValues(narrativeBundle)...)
		}
	}
	if fieldsPayload, ok := payload["fields"].(map[string]any); ok {
		searchValues = append(searchValues, flattenNormalizedFieldValues(fieldsPayload)...)
	}

	return normalizeChatSearchValue(canonicalName),
		normalizeChatSearchValue(canonicalCommunity),
		normalizeChatSearchValue(canonicalSchool),
		normalizeChatSearchValue(strings.Join(searchValues, " "))
}

func flattenStringValues(value any) []string {
	switch v := value.(type) {
	case map[string]any:
		out := make([]string, 0, len(v))
		for _, child := range v {
			out = append(out, flattenStringValues(child)...)
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, child := range v {
			out = append(out, flattenStringValues(child)...)
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

func normalizedChatRowJSON(defaultBundle map[string]any, options ...func(map[string]any)) string {
	payload := map[string]any{
		"chat": map[string]any{
			"default_bundle": defaultBundle,
		},
	}
	for _, option := range options {
		option(payload)
	}
	out, _ := json.Marshal(payload)
	return string(out)
}

func withCanonical(values map[string]any) func(map[string]any) {
	return func(payload map[string]any) {
		payload["canonical"] = values
	}
}

func withFields(values map[string]any) func(map[string]any) {
	return func(payload map[string]any) {
		payload["fields"] = values
	}
}

func withNarrative(values map[string]any) func(map[string]any) {
	return func(payload map[string]any) {
		chatPayload, _ := payload["chat"].(map[string]any)
		if chatPayload == nil {
			chatPayload = map[string]any{}
			payload["chat"] = chatPayload
		}
		chatPayload["narrative_bundle"] = values
	}
}

func flattenNormalizedFieldValues(fields map[string]any) []string {
	out := make([]string, 0, len(fields)*2)
	for _, rawField := range fields {
		field, ok := rawField.(map[string]any)
		if !ok {
			continue
		}
		if raw, ok := field["raw"].(string); ok && strings.TrimSpace(raw) != "" {
			out = append(out, raw)
		}
		if normalized, ok := field["normalized"].(string); ok && strings.TrimSpace(normalized) != "" {
			out = append(out, normalized)
		}
		if tokens, ok := field["tokens"].([]any); ok {
			for _, token := range tokens {
				if text, ok := token.(string); ok && strings.TrimSpace(text) != "" {
					out = append(out, text)
				}
			}
		}
	}
	return out
}

func jsonStructuredAnswer(answer string, matchedRowRef *string) *genai.GenerateContentResponse {
	payload := map[string]any{
		"answer":          answer,
		"matched_row_ref": nil,
	}
	if matchedRowRef != nil {
		payload["matched_row_ref"] = *matchedRowRef
	}

	text, _ := json.Marshal(payload)

	var out genai.GenerateContentResponse
	_ = json.Unmarshal([]byte(`{"candidates":[{"content":{"parts":[{"text":`+strconvQuote(string(text))+`}]}}]}`), &out)
	return &out
}

func rawAnswer(text string) *genai.GenerateContentResponse {
	var out genai.GenerateContentResponse
	_ = json.Unmarshal([]byte(`{"candidates":[{"content":{"parts":[{"text":`+strconvQuote(text)+`}]}}]}`), &out)
	return &out
}

func verifiedRoutedAnswer(t *testing.T, contents []*genai.Content) *genai.GenerateContentResponse {
	payload := routedAnswerPayloadFromPrompt(t, contents)
	return rawAnswer(payload.VerifiedQueryResult)
}

func routedAnswerPayloadFromPrompt(t *testing.T, contents []*genai.Content) routedAnswerPayload {
	t.Helper()
	if len(contents) == 0 || len(contents[0].Parts) == 0 {
		t.Fatal("expected routed answer prompt")
	}
	prompt := contents[0].Parts[0].Text
	const marker = "INPUT JSON:\n"
	markerIndex := strings.LastIndex(prompt, marker)
	if markerIndex < 0 {
		t.Fatalf("expected verified query result payload in prompt, got:\n%s", prompt)
	}
	var payload routedAnswerPayload
	if err := json.Unmarshal([]byte(prompt[markerIndex+len(marker):]), &payload); err != nil {
		t.Fatalf("decode routed answer payload: %v", err)
	}
	if strings.TrimSpace(payload.VerifiedQueryResult) == "" {
		t.Fatal("expected non-empty verified query result")
	}
	return payload
}

func strconvQuote(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestFirstTextFromResp_ReturnsFirstNonEmptyPart(t *testing.T) {
	var resp genai.GenerateContentResponse
	_ = json.Unmarshal([]byte(`{
		"candidates":[
			{"content":{"parts":[
				{"text":"first"},
				{"text":"second"}
			]}}
		]
	}`), &resp)

	if got := firstTextFromResp(&resp); got != "first" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveChatResponse_ParsesMatchedRowID(t *testing.T) {
	rowRef := "r2"
	answer, matchedRowID := resolveChatResponse(
		`{"answer":"Alice attended Shingwauk.","matched_row_ref":"`+rowRef+`"}`,
		map[string]int{"R1": 11, "R2": 22},
	)

	if answer != "Alice attended Shingwauk." {
		t.Fatalf("answer = %q", answer)
	}
	if matchedRowID == nil || *matchedRowID != 22 {
		t.Fatalf("matched row = %#v", matchedRowID)
	}
}

func TestResolveChatResponse_FallsBackToRawText(t *testing.T) {
	answer, matchedRowID := resolveChatResponse("Plain text answer.", map[string]int{"R1": 11})
	if answer != "Plain text answer." {
		t.Fatalf("answer = %q", answer)
	}
	if matchedRowID != nil {
		t.Fatalf("matched row = %#v", matchedRowID)
	}
}

func TestResolveChatResponse_RecoversAnswerFromMalformedStructuredJSON(t *testing.T) {
	raw := `{"answer":"Garden River has the largest number of listed survivors.","matched_row_ref":null}`
	// Simulate the occasional unescaped quote that makes an otherwise standard
	// structured response invalid JSON.
	raw = strings.Replace(raw, `survivors.",`, `survivors".",`, 1)

	answer, matchedRowID := resolveChatResponse(raw, nil)
	if strings.Contains(answer, `"answer"`) || strings.Contains(answer, `"matched_row_ref"`) {
		t.Fatalf("structured response leaked into display answer: %q", answer)
	}
	if !strings.Contains(answer, "Garden River") {
		t.Fatalf("expected recovered human-readable answer, got %q", answer)
	}
	if matchedRowID != nil {
		t.Fatalf("matched row = %#v", matchedRowID)
	}
}

func TestResolveChatResponse_NeverReturnsRawJSONObject(t *testing.T) {
	answer, _ := resolveChatResponse(`{"unexpected":"shape"}`, nil)
	if strings.HasPrefix(strings.TrimSpace(answer), "{") || strings.Contains(answer, `"unexpected"`) {
		t.Fatalf("raw JSON leaked into display answer: %q", answer)
	}
}

func TestChatService_Chat_FileNotFound(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	mock.ExpectQuery(`(?i)select.*id.*version.*from.*file.*where.*filename.*order.*version.*desc.*limit`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "version", "description"}))

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	_, err := cs.Chat("q", nil, "missing.xlsx", nil)
	if err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("expected file not found, got %v", err)
	}
}

func TestChatService_Chat_UsesStructuredPrompt(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(mock, normalizedChatRowJSON(
		map[string]any{
			"name":      "Alice",
			"community": "Garden River",
		},
		withCanonical(map[string]any{
			"display_name": "Alice",
			"community":    "Garden River",
		}),
	))

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if model != "gemini-2.5-flash" {
			t.Fatalf("unexpected generate model %q", model)
		}
		if config != nil {
			t.Fatalf("expected nil config, got %#v", config)
		}
		if len(contents) == 0 || contents[0] == nil || len(contents[0].Parts) == 0 {
			t.Fatalf("expected prompt contents, got %#v", contents)
		}

		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, "Structured output requirements:") {
			t.Fatalf("expected structured output instructions, got:\n%s", prompt)
		}
		if !strings.Contains(prompt, "default_bundle") {
			t.Fatalf("expected compact bundle data, got:\n%s", prompt)
		}
		if !strings.Contains(prompt, `"matched_row_ref": "R1" or null`) {
			t.Fatalf("expected matched_row_ref guidance, got:\n%s", prompt)
		}
		if !strings.Contains(prompt, `"row_ref":"R1"`) {
			t.Fatalf("expected prompt to include row refs, got:\n%s", prompt)
		}

		return jsonStructuredAnswer("Yes. I found 1 recorded death connected to drowning at Shingwauk.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Did any children die from drowning at Shingwauk?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "Yes. I found 1 recorded death connected to drowning at Shingwauk." {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.MatchedRowID != nil {
		t.Fatalf("unexpected matched row: %#v", result.MatchedRowID)
	}
}

func TestChatService_Chat_ReturnsMatchedRowID(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(mock, normalizedChatRowJSON(
		map[string]any{
			"name":      "Alice",
			"community": "Garden River",
		},
		withCanonical(map[string]any{
			"display_name": "Alice",
			"community":    "Garden River",
		}),
	))

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	rowRef := "R1"
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, _ []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if config != nil {
			t.Fatalf("expected nil config, got %#v", config)
		}
		return jsonStructuredAnswer("Alice is from Garden River.", &rowRef), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Who is Alice?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.MatchedRowID == nil || *result.MatchedRowID != 1 {
		t.Fatalf("unexpected matched row: %#v", result.MatchedRowID)
	}
}

func TestChatService_Chat_StructuredEntityLookupNarrowsPromptRows(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Alice Johnson",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Alice Johnson",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Bob Thomas",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Bob Thomas",
				"community":    "Garden River",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	rowRef := "R1"
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, "Alice Johnson") {
			t.Fatalf("expected Alice row in prompt, got:\n%s", prompt)
		}
		if strings.Contains(prompt, "Bob Thomas") {
			t.Fatalf("did not expect Bob row in entity lookup prompt, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("Alice Johnson is from Garden River.", &rowRef), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Who is Alice Johnson?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "entity_rows" {
		t.Fatalf("expected entity retrieval mode, got %#v", result.Debug)
	}
}

func TestChatService_Chat_FallsBackToProOn429(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(mock, normalizedChatRowJSON(
		map[string]any{
			"name": "Alice",
		},
		withCanonical(map[string]any{
			"display_name": "Alice",
		}),
	))

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, _ []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if config != nil {
			t.Fatalf("expected nil config, got %#v", config)
		}
		if model == "gemini-2.5-flash" {
			return nil, &googleapi.Error{Code: http.StatusTooManyRequests}
		}
		if model != "gemini-2.5-pro" {
			t.Fatalf("unexpected fallback model %q", model)
		}
		return jsonStructuredAnswer("Based on the records provided, the highest number of deaths occurred in 1913.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("In what years did the highest number of deaths occur?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "1913") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.Debug == nil {
		t.Fatal("expected debug metrics")
	}
	if result.Debug.UsedModel != "gemini-2.5-pro" {
		t.Fatalf("used model = %q want gemini-2.5-pro", result.Debug.UsedModel)
	}
}

func TestChatService_Chat_PopulatesDebugMetrics(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Alice",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Alice",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Bob",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Bob",
				"community":    "Garden River",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return jsonStructuredAnswer("Garden River has two matching rows.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Who is from Garden River?", nil, "sheet.xlsx", []string{"Garden River"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil {
		t.Fatal("expected debug metrics")
	}
	if result.Debug.Strategy != "structured_retrieval" {
		t.Fatalf("strategy = %q want structured_retrieval", result.Debug.Strategy)
	}
	if result.Debug.TotalRowsLoaded != 2 {
		t.Fatalf("total rows loaded = %d want 2", result.Debug.TotalRowsLoaded)
	}
	if result.Debug.RowsSelected != 2 {
		t.Fatalf("rows selected = %d want 2", result.Debug.RowsSelected)
	}
	if result.Debug.CommunityFilterCount != 1 {
		t.Fatalf("community filter count = %d want 1", result.Debug.CommunityFilterCount)
	}
	if result.Debug.RetrievalMode != "keyword_rows" {
		t.Fatalf("retrieval mode = %q want keyword_rows", result.Debug.RetrievalMode)
	}
	if result.Debug.PromptProjectionMode != "full" {
		t.Fatalf("prompt projection mode = %q want full", result.Debug.PromptProjectionMode)
	}
	if result.Debug.PromptChars <= 0 || result.Debug.PromptBytes <= 0 {
		t.Fatalf("expected positive prompt sizing, got %#v", result.Debug)
	}
	if result.Debug.PrimaryModel != "gemini-2.5-flash" {
		t.Fatalf("primary model = %q want gemini-2.5-flash", result.Debug.PrimaryModel)
	}
	if result.Debug.UsedModel != "gemini-2.5-flash" {
		t.Fatalf("used model = %q want gemini-2.5-flash", result.Debug.UsedModel)
	}
}

func TestChatService_Chat_CompressesBroadStructuredPrompt(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	rows := []string{
		normalizedChatRowJSON(
			map[string]any{
				"name":             "Alice Johnson",
				"community":        "Garden River",
				"school":           "Shingwauk",
				"parents_names":    "John and Mary Johnson",
				"mapping_location": "Garden River, Ontario",
			},
			withCanonical(map[string]any{
				"display_name": "Alice Johnson",
				"community":    "Garden River",
				"school":       "Shingwauk",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":             "Bob Thomas",
				"community":        "Garden River",
				"school":           "Shingwauk",
				"parents_names":    "Peter and Alice Thomas",
				"mapping_location": "Garden River, Ontario",
			},
			withCanonical(map[string]any{
				"display_name": "Bob Thomas",
				"community":    "Garden River",
				"school":       "Shingwauk",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":             "Carol Pine",
				"community":        "Garden River",
				"school":           "Shingwauk",
				"parents_names":    "David and Ruth Pine",
				"mapping_location": "Garden River, Ontario",
			},
			withCanonical(map[string]any{
				"display_name": "Carol Pine",
				"community":    "Garden River",
				"school":       "Shingwauk",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":             "Daniel Roy",
				"community":        "Garden River",
				"school":           "Shingwauk",
				"parents_names":    "Emma Roy",
				"mapping_location": "Garden River, Ontario",
			},
			withCanonical(map[string]any{
				"display_name": "Daniel Roy",
				"community":    "Garden River",
				"school":       "Shingwauk",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":             "Eva White",
				"community":        "Garden River",
				"school":           "Shingwauk",
				"parents_names":    "Henry and Lucy White",
				"mapping_location": "Garden River, Ontario",
			},
			withCanonical(map[string]any{
				"display_name": "Eva White",
				"community":    "Garden River",
				"school":       "Shingwauk",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":             "Frank Stone",
				"community":        "Garden River",
				"school":           "Shingwauk",
				"parents_names":    "Samuel and Grace Stone",
				"mapping_location": "Garden River, Ontario",
			},
			withCanonical(map[string]any{
				"display_name": "Frank Stone",
				"community":    "Garden River",
				"school":       "Shingwauk",
			}),
		),
	}
	expectChatNormalizedRowsLookup(mock, rows...)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, `"community":"Garden River"`) {
			t.Fatalf("expected compressed prompt to keep community, got:\n%s", prompt)
		}
		if strings.Contains(prompt, `"parents_names":`) {
			t.Fatalf("did not expect broad compressed prompt to keep parents_names, got:\n%s", prompt)
		}
		if strings.Contains(prompt, `"mapping_location":`) {
			t.Fatalf("did not expect broad compressed prompt to keep mapping_location, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("Garden River has multiple matching students.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Who is from Garden River?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.PromptProjectionMode != "relevant_only" {
		t.Fatalf("expected relevant_only prompt projection, got %#v", result.Debug)
	}
}

func TestChatService_Chat_KeepsFullPromptForEntityQuestion(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(mock, normalizedChatRowJSON(
		map[string]any{
			"name":             "Alice Johnson",
			"community":        "Garden River",
			"parents_names":    "John and Mary Johnson",
			"mapping_location": "Garden River, Ontario",
		},
		withCanonical(map[string]any{
			"display_name": "Alice Johnson",
			"community":    "Garden River",
		}),
	))

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, `"parents_names":"John and Mary Johnson"`) {
			t.Fatalf("expected full entity prompt to keep parents_names, got:\n%s", prompt)
		}
		if !strings.Contains(prompt, `"mapping_location":"Garden River, Ontario"`) {
			t.Fatalf("expected full entity prompt to keep mapping_location, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("Alice Johnson is from Garden River.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Who is Alice Johnson?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.PromptProjectionMode != "full" {
		t.Fatalf("expected full prompt projection, got %#v", result.Debug)
	}
}

func TestChatService_Chat_IncludesSourceFieldsForArbitraryColumnQuestion(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(mock, normalizedChatRowJSON(
		map[string]any{
			"name":      "Alice Johnson",
			"community": "Garden River",
		},
		withCanonical(map[string]any{
			"display_name": "Alice Johnson",
			"community":    "Garden River",
		}),
		withFields(map[string]any{
			"Age": map[string]any{
				"raw":        "14",
				"normalized": "14",
				"role":       "text",
				"tokens":     []any{"14"},
			},
			"Siblings": map[string]any{
				"raw":        "Mary and John",
				"normalized": "mary and john",
				"role":       "text",
				"tokens":     []any{"mary", "john"},
			},
		}),
	))

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, `"source_fields":{`) || !strings.Contains(prompt, `"Age":"14"`) {
			t.Fatalf("expected arbitrary source field in prompt, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("Alice Johnson is listed as age 14.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("What age was Alice Johnson?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.PromptProjectionMode != "full" {
		t.Fatalf("expected full prompt projection for single-row arbitrary column question, got %#v", result.Debug)
	}
}

func TestChatService_Chat_IncludesSourceFieldsWhenValueMatchOnlyExistsInRawColumn(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Alice Johnson",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Alice Johnson",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Raymond Fisher",
				"community":       "Garden River",
				"deceased_status": "Yes",
			},
			withCanonical(map[string]any{
				"display_name":    "Raymond Fisher",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
			withFields(map[string]any{
				"Incident Type": map[string]any{
					"raw":        "Drowning",
					"normalized": "drowning",
					"role":       "text",
					"tokens":     []any{"drowning", "drown"},
				},
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, `"source_fields":{`) || !strings.Contains(prompt, `"Incident Type":"Drowning"`) {
			t.Fatalf("expected raw source field carrying the drowning match, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("Yes. One matched record indicates drowning.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Did any children die from drowning?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.RetrievalMode == "legacy_full_dataset_fallback" {
		t.Fatalf("expected structured retrieval path, got %#v", result.Debug)
	}
}

func TestChatService_Chat_AudioReadError(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(mock, normalizedChatRowJSON(
		map[string]any{"name": "Alice"},
		withCanonical(map[string]any{"display_name": "Alice"}),
	))

	oldRead := readAllHook
	readAllHook = func(_ io.Reader) ([]byte, error) {
		return nil, errors.New("read fail")
	}
	t.Cleanup(func() { readAllHook = oldRead })

	fh := makeAudioFileHeader(t, "audio/webm", []byte("xxx"))
	cs := &ChatService{DB: db, Client: &genai.Client{}}
	_, err := cs.Chat("hello", fh, "sheet.xlsx", nil)
	if err == nil || !strings.Contains(err.Error(), "failed to read audio file") {
		t.Fatalf("expected audio read error, got %v", err)
	}
}

func TestChatService_Chat_IncludesNarrativeBundleForNarrativeKeywordMatches(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name": "Alice",
			},
			withCanonical(map[string]any{
				"display_name": "Alice",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Raymond Fisher",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Raymond Fisher",
				"community":    "Garden River",
			}),
			withNarrative(map[string]any{
				"notes": "Drowning was mentioned in the school correspondence.",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, `"narrative_bundle":{`) || !strings.Contains(prompt, "Drowning was mentioned in the school correspondence.") {
			t.Fatalf("expected narrative bundle in prompt, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("Yes. One matched record mentions drowning.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Did any children die from drowning at Shingwauk?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.NarrativeRowsIncluded != 1 {
		t.Fatalf("expected one narrative row, got %#v", result.Debug)
	}
}

func TestChatService_Chat_MatchesDrownedNarrativeForDrowningQuestion(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name": "Alice",
			},
			withCanonical(map[string]any{
				"display_name": "Alice",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Raymond Fisher",
				"community":       "Garden River",
				"deceased_status": "Yes",
			},
			withCanonical(map[string]any{
				"display_name":    "Raymond Fisher",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
			withNarrative(map[string]any{
				"notes": "He drowned while returning home from school.",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, `"narrative_bundle":{`) || !strings.Contains(prompt, "He drowned while returning home from school.") {
			t.Fatalf("expected drowned narrative variant in prompt, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("Yes. One matched record describes a child who drowned.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Did any children die from drowning at Shingwauk?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.NarrativeRowsIncluded != 1 {
		t.Fatalf("expected one narrative row, got %#v", result.Debug)
	}
}

func TestChatService_Chat_DeathYearQuestionIncludesDeathNarrativeAndDatasetContext(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	filename := "Shingwauk And Wawanosh Students Master List"
	expectChatFileLookup(mock, filename, "Master list spanning Shingwauk and Wawanosh records, including obituary and death details where available.")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":              "Alice Johnson",
				"community":         "Garden River",
				"deceased_status":   "Yes",
				"has_death_details": true,
			},
			withCanonical(map[string]any{
				"display_name":    "Alice Johnson",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
			withNarrative(map[string]any{
				"death_details": "Died 13 Jan 1913, obituary.",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":              "Bob Thomas",
				"community":         "Batchawana Bay",
				"deceased_status":   "Yes",
				"has_death_details": true,
			},
			withCanonical(map[string]any{
				"display_name":    "Bob Thomas",
				"community":       "Batchawana Bay",
				"deceased_status": "yes",
			}),
			withNarrative(map[string]any{
				"death_details": "Died 2 Dec 1913, grave marker.",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":              "Carol Pine",
				"community":         "Garden River",
				"deceased_status":   "Yes",
				"has_death_details": true,
			},
			withCanonical(map[string]any{
				"display_name":    "Carol Pine",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
			withNarrative(map[string]any{
				"death_details": "Died 4 Mar 1914, church record.",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, "Dataset context:") || !strings.Contains(prompt, filename) {
			t.Fatalf("expected dataset context in prompt, got:\n%s", prompt)
		}
		if !strings.Contains(prompt, "Master list spanning Shingwauk and Wawanosh records") {
			t.Fatalf("expected file description in prompt, got:\n%s", prompt)
		}
		if !strings.Contains(prompt, `"narrative_bundle":{`) || !strings.Contains(prompt, "Died 13 Jan 1913, obituary.") || !strings.Contains(prompt, "Died 2 Dec 1913, grave marker.") {
			t.Fatalf("expected death narrative details in prompt, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("The highest number of recorded deaths occurred in 1913.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("In what years did the highest number of deaths occur at Shingwauk?", nil, filename, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil {
		t.Fatal("expected debug metrics")
	}
	if result.Debug.PromptProjectionMode != "full" {
		t.Fatalf("expected full prompt projection for death-year question, got %#v", result.Debug)
	}
	if result.Debug.NarrativeRowsIncluded != 3 {
		t.Fatalf("expected all death rows to include narrative, got %#v", result.Debug)
	}
}

func TestChatService_Chat_FallsBackToFullDatasetWhenStructuredRowsUnavailable(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	mock.ExpectQuery(`(?i)select.*source_row_id.*canonical_name.*canonical_community.*canonical_school.*search_text.*row_data_normalized.*from.*file_data_normalized.*where.*file_id.*version.*status.*normalization_version.*order.*source_row_id.*asc`).
		WillReturnRows(sqlmock.NewRows([]string{"source_row_id", "canonical_name", "canonical_community", "canonical_school", "search_text", "row_data_normalized"}))
	mock.ExpectQuery(`(?i)select.*source_row_id.*canonical_name.*canonical_community.*canonical_school.*search_text.*row_data_normalized.*from.*file_data_normalized.*where.*file_id.*version.*status.*normalization_version.*order.*source_row_id.*asc`).
		WillReturnRows(sqlmock.NewRows([]string{"source_row_id", "canonical_name", "canonical_community", "canonical_school", "search_text", "row_data_normalized"}))
	expectChatRowsLookup(mock, `{"NAME":"Alice","First Nation/Community":"Garden River"}`)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, `"row_data":{"NAME":"Alice","First Nation/Community":"Garden River"}`) {
			t.Fatalf("expected legacy row_data fallback prompt, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("Alice is from Garden River.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Who is Alice?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "legacy_full_dataset_fallback" {
		t.Fatalf("expected legacy fallback mode, got %#v", result.Debug)
	}
}

func TestChatService_Chat_RoutesCountQueryDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Alice Johnson",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Alice Johnson",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Bob Thomas",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Bob Thomas",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Carol Pine",
				"community": "Batchawana Bay",
			},
			withCanonical(map[string]any{
				"display_name": "Carol Pine",
				"community":    "Batchawana Bay",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if model != routedAnswerFastModel {
			t.Fatalf("model = %q want %q", model, routedAnswerFastModel)
		}
		if config == nil || config.Temperature == nil || *config.Temperature != 0 || config.CandidateCount != 1 ||
			config.MaxOutputTokens < minRoutedAnswerOutputTokens || config.ResponseMIMEType != "text/plain" || config.SystemInstruction == nil {
			t.Fatalf("unexpected routed generation config: %#v", config)
		}
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("How many students are from Garden River?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "2") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.Debug == nil {
		t.Fatal("expected debug metrics")
	}
	if result.Debug.Strategy != "deterministic_router" {
		t.Fatalf("strategy = %q want deterministic_router", result.Debug.Strategy)
	}
	if result.Debug.ExecutionMode != "deterministic_llm" {
		t.Fatalf("execution mode = %q want deterministic_llm", result.Debug.ExecutionMode)
	}
	if result.Debug.QueryType != "count" {
		t.Fatalf("query type = %q want count", result.Debug.QueryType)
	}
	if result.Debug.PromptBytes == 0 || result.Debug.UsedModel != routedAnswerFastModel {
		t.Fatalf("expected compact Flash prompt for deterministic route, got %#v", result.Debug)
	}
}

func TestChatService_Chat_RoutesFieldLookupDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":           "Alice Johnson",
				"student_number": "1940.442",
				"community":      "Garden River",
			},
			withCanonical(map[string]any{
				"display_name":       "Alice Johnson",
				"student_number":     "1940-442",
				"student_number_raw": "1940.442",
				"community":          "Garden River",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("What is Alice Johnson's student number?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "1940.442") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.MatchedRowID == nil || *result.MatchedRowID != 1 {
		t.Fatalf("unexpected matched row: %#v", result.MatchedRowID)
	}
	if result.Debug == nil || result.Debug.QueryType != "field_lookup" {
		t.Fatalf("expected deterministic field lookup debug, got %#v", result.Debug)
	}
}

func TestChatService_Chat_RoutesGroupExtremeDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Alice Johnson",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Alice Johnson",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Bob Thomas",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Bob Thomas",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Carol Pine",
				"community": "Batchawana Bay",
			},
			withCanonical(map[string]any{
				"display_name": "Carol Pine",
				"community":    "Batchawana Bay",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Which community has the most students?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "Garden River") || !strings.Contains(result.Answer, "2") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.Debug == nil || result.Debug.QueryType != "group_extreme" {
		t.Fatalf("expected deterministic group_extreme debug, got %#v", result.Debug)
	}
}

func TestChatService_Chat_RoutesDeathGroupExtremeDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Alice Johnson",
				"community":       "Garden River",
				"deceased_status": "Yes",
			},
			withCanonical(map[string]any{
				"display_name":    "Alice Johnson",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Bob Thomas",
				"community":       "Garden River",
				"deceased_status": "Yes",
			},
			withCanonical(map[string]any{
				"display_name":    "Bob Thomas",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Carol Pine",
				"community":       "Batchawana Bay",
				"deceased_status": "Yes",
			},
			withCanonical(map[string]any{
				"display_name":    "Carol Pine",
				"community":       "Batchawana Bay",
				"deceased_status": "yes",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Daniel Roy",
				"community":       "Batchawana Bay",
				"deceased_status": "No",
			},
			withCanonical(map[string]any{
				"display_name":    "Daniel Roy",
				"community":       "Batchawana Bay",
				"deceased_status": "no",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Which community has highest number of deaths?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "Garden River") || !strings.Contains(result.Answer, "2") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.Debug == nil || result.Debug.QueryType != "group_extreme" {
		t.Fatalf("expected deterministic group_extreme debug, got %#v", result.Debug)
	}
}

func TestChatService_Chat_RoutesDeathGroupExtremeWithNamesDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Alice Johnson",
				"community":       "Garden River",
				"deceased_status": "Yes",
			},
			withCanonical(map[string]any{
				"display_name":    "Alice Johnson",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Bob Thomas",
				"community":       "Garden River",
				"deceased_status": "Yes",
			},
			withCanonical(map[string]any{
				"display_name":    "Bob Thomas",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":            "Carol Pine",
				"community":       "Batchawana Bay",
				"deceased_status": "Yes",
			},
			withCanonical(map[string]any{
				"display_name":    "Carol Pine",
				"community":       "Batchawana Bay",
				"deceased_status": "yes",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("What community had the most deaths, and what are the names of those children?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "Garden River") || !strings.Contains(result.Answer, "Alice Johnson") || !strings.Contains(result.Answer, "Bob Thomas") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if strings.Contains(result.Answer, "Carol Pine") {
		t.Fatalf("expected only top community names, got %#v", result)
	}
	if result.Debug == nil || result.Debug.QueryType != "group_extreme" {
		t.Fatalf("expected deterministic group_extreme debug, got %#v", result.Debug)
	}
}

func TestChatService_Chat_RoutesDeathDatasetCommunityExtremeDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	filename := "Confirmed- Shingwauk (Wawanosh)"
	expectChatFileLookup(mock, filename)
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":          "Andrew Johnson",
				"community":     "Walpole Island",
				"school":        "Shingwauk, Sault Ste. Marie, ON",
				"date_of_death": "1906-03-07",
			},
			withCanonical(map[string]any{
				"display_name": "Andrew Johnson",
				"community":    "Walpole Island",
				"school":       "Shingwauk, Sault Ste. Marie, ON",
				"dates": map[string]any{
					"death": map[string]any{
						"raw":  "1906-03-07",
						"iso":  "1906-03-07",
						"year": 1906,
					},
				},
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":          "Albert Penance",
				"community":     "Walpole Island",
				"school":        "Shingwauk, Sault Ste. Marie, ON",
				"date_of_death": "1903-07-30",
			},
			withCanonical(map[string]any{
				"display_name": "Albert Penance",
				"community":    "Walpole Island",
				"school":       "Shingwauk, Sault Ste. Marie, ON",
				"dates": map[string]any{
					"death": map[string]any{
						"raw":  "1903-07-30",
						"iso":  "1903-07-30",
						"year": 1903,
					},
				},
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":          "Freida Augustine",
				"community":     "Garden River",
				"school":        "Shingwauk, Sault Ste. Marie, ON",
				"date_of_death": "1913-04-17",
			},
			withCanonical(map[string]any{
				"display_name": "Freida Augustine",
				"community":    "Garden River",
				"school":       "Shingwauk, Sault Ste. Marie, ON",
				"dates": map[string]any{
					"death": map[string]any{
						"raw":  "1913-04-17",
						"iso":  "1913-04-17",
						"year": 1913,
					},
				},
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("What community / reserve had the most deaths? What are the names of those children?", nil, filename, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "Walpole Island") || !strings.Contains(result.Answer, "Andrew Johnson") || !strings.Contains(result.Answer, "Albert Penance") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.Debug == nil || result.Debug.QueryType != "group_extreme" {
		t.Fatalf("expected deterministic group_extreme debug, got %#v", result.Debug)
	}
}

func TestChatService_Chat_RoutesLowestGroupExtremeDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Alice Johnson",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Alice Johnson",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Bob Thomas",
				"community": "Garden River",
			},
			withCanonical(map[string]any{
				"display_name": "Bob Thomas",
				"community":    "Garden River",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":      "Carol Pine",
				"community": "Batchawana Bay",
			},
			withCanonical(map[string]any{
				"display_name": "Carol Pine",
				"community":    "Batchawana Bay",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Which community has the fewest students?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "Batchawana Bay") || !strings.Contains(result.Answer, "1") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.Debug == nil || result.Debug.QueryType != "group_extreme" {
		t.Fatalf("expected deterministic group_extreme debug, got %#v", result.Debug)
	}
}

func TestChatService_Chat_ListsAllCommunitiesDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	communityNames := []string{
		"Walpole Island",
		"Garden River",
		"Batchawana Bay",
		"Sagamok",
		"Serpent River",
		"Thessalon",
		"Nipissing",
		"Dokis",
		"Whitefish River",
		"Garden River",
	}
	rows := make([]string, 0, len(communityNames))
	for idx, community := range communityNames {
		rows = append(rows, normalizedChatRowJSON(
			map[string]any{
				"name":      "Student " + strconv.Itoa(idx+1),
				"community": community,
			},
			withCanonical(map[string]any{
				"display_name": "Student " + strconv.Itoa(idx+1),
				"community":    community,
			}),
		))
	}
	expectChatNormalizedRowsLookup(mock, rows...)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Give me all communities in the list", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil || result.Debug.QueryType != "distinct_values" || result.Debug.ExecutionMode != "deterministic_llm" {
		t.Fatalf("expected deterministic distinct-values debug, got %#v", result.Debug)
	}
	if result.Debug.PromptBytes == 0 || result.Debug.UsedModel != routedAnswerFastModel {
		t.Fatalf("expected compact Flash answer prompt, got %#v", result.Debug)
	}
	if !strings.Contains(result.Answer, "The 9 communities in the data are") {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	for _, community := range communityNames[:9] {
		if !strings.Contains(result.Answer, community) {
			t.Fatalf("expected %q in answer: %q", community, result.Answer)
		}
	}
	if strings.Count(result.Answer, "Garden River") != 1 {
		t.Fatalf("expected duplicate communities to be removed: %q", result.Answer)
	}
	if strings.Index(result.Answer, "Batchawana Bay") > strings.Index(result.Answer, "Dokis") ||
		strings.Index(result.Answer, "Dokis") > strings.Index(result.Answer, "Garden River") {
		t.Fatalf("expected communities to be sorted alphabetically: %q", result.Answer)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_ListsConfiguredCanonicalSchoolsWithinCommunityFilter(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{"name": "Alice", "community": "Garden River", "school": "Shingwauk"},
			withCanonical(map[string]any{"display_name": "Alice", "community": "Garden River", "school": "Shingwauk"}),
		),
		normalizedChatRowJSON(
			map[string]any{"name": "Bob", "community": "Batchawana Bay", "school": "Wawanosh"},
			withCanonical(map[string]any{"display_name": "Bob", "community": "Batchawana Bay", "school": "Wawanosh"}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("What schools are in the data?", nil, "sheet.xlsx", []string{"Garden River"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "The school in the data is Shingwauk." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.QueryType != "distinct_values" || result.Debug.RowsSelected != 1 {
		t.Fatalf("expected filtered deterministic distinct-values debug, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestLooksLikeDeterministicDistinctValueListQuestion_DoesNotStealDetailRequests(t *testing.T) {
	if looksLikeDeterministicDistinctValueListQuestion("give me details about alice's community") {
		t.Fatal("expected a person detail request to remain on the normal lookup path")
	}
	if !looksLikeDeterministicDistinctValueListQuestion("give me all communities in the list") {
		t.Fatal("expected an explicit distinct-value request to use the deterministic path")
	}

	rows := []cachedStructuredChatRow{
		{
			CanonicalName:   "alice",
			CanonicalSchool: "shingwauk",
			DefaultBundle: structuredChatDefaultBundle{
				Name:   "Alice",
				School: "Shingwauk",
			},
		},
	}
	ctx := buildDeterministicQuestionContext(rows, "Give me all schools attended by Alice")
	if _, ok := tryDeterministicDistinctGroupValuesRoute(rows, nil, ctx); ok {
		t.Fatal("expected a person-specific school request not to become a whole-dataset distinct list")
	}
}

func TestChatService_Chat_ListsDataConfigFieldValuesDeterministically(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	mock.ExpectQuery(`(?i)select.*config.*from.*data_config.*where.*file_id.*is_active.*order.*updated_at.*id.*limit`).
		WithArgs(int64(1), true, 1).
		WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow(`{
			"source_file": {
				"data_config": {
					"fields": [
						{"key":"first_language","label":"First Language","source_field":"First Language"}
					]
				}
			}
		}`))
	expectDatabaseConfiguredValues(mock,
		databaseConfiguredValue{Display: "Anishinaabemowin", Count: 1},
		databaseConfiguredValue{Display: "Cree", Count: 2},
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Give me all first languages in the list", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "I found 2 distinct values for First Language: Anishinaabemowin and Cree." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.QueryType != "distinct_values" || result.Debug.RetrievalMode != "database_config_distinct_values" {
		t.Fatalf("expected configured deterministic distinct-values debug, got %#v", result.Debug)
	}
	if result.Debug.PromptBytes == 0 || result.Debug.UsedModel != routedAnswerFastModel || result.Debug.ExecutionMode != "database_llm" {
		t.Fatalf("expected configured answer rendered by Flash, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersFilteredCountFromDatabase(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectDatabaseDimensionValues(mock, "community",
		databaseDimensionValue{Normalized: "garden river", Display: "Garden River", Count: 2},
		databaseDimensionValue{Normalized: "batchawana bay", Display: "Batchawana Bay", Count: 1},
	)
	expectDatabaseDimensionValues(mock, "school",
		databaseDimensionValue{Normalized: "shingwauk", Display: "Shingwauk", Count: 3},
	)
	expectDatabaseCount(mock, 2)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if model != routedAnswerFastModel {
			t.Fatalf("model = %q want %q", model, routedAnswerFastModel)
		}
		_ = verifiedRoutedAnswer(t, contents)
		return rawAnswer("There are 2 students from Garden River."), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("How many students are from Garden River?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "There are 2 students from Garden River." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.Strategy != "database_router" || result.Debug.RetrievalMode != "database_count" {
		t.Fatalf("expected database count route, got %#v", result.Debug)
	}
	if result.Debug.PromptBytes == 0 || result.Debug.UsedModel != routedAnswerFastModel || result.Debug.ExecutionMode != "database_llm" {
		t.Fatalf("expected database result rendered by Flash, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersExistenceFromDatabase(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectDatabaseDimensionValues(mock, "community",
		databaseDimensionValue{Normalized: "garden river", Display: "Garden River", Count: 2},
	)
	expectDatabaseDimensionValues(mock, "school")
	expectDatabaseCount(mock, 2)

	cs := &ChatService{DB: db}
	result, err := cs.Chat("Are there any students from Garden River?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "Yes, I found 2 matching records." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "database_existence" {
		t.Fatalf("expected database existence route, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersDistinctCommunityListFromDatabase(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	communityValues := []databaseDimensionValue{
		{Normalized: "garden river", Display: "Garden River", Count: 2},
		{Normalized: "batchawana bay", Display: "Batchawana Bay", Count: 1},
		{Normalized: "walpole island", Display: "Walpole Island", Count: 3},
	}
	expectDatabaseDimensionValues(mock, "community", communityValues...)

	cs := &ChatService{DB: db}
	result, err := cs.Chat("Give me all communities in the list", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "The 3 communities in the data are Batchawana Bay, Garden River, and Walpole Island." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.Strategy != "database_router" || result.Debug.RetrievalMode != "database_distinct_values" || result.Debug.RowsSelected != 6 {
		t.Fatalf("expected database distinct-values route, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersDistinctCommunityCountFromDatabase(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	communityValues := []databaseDimensionValue{
		{Normalized: "garden river", Display: "Garden River", Count: 2},
		{Normalized: "batchawana bay", Display: "Batchawana Bay", Count: 1},
	}
	expectDatabaseDimensionValues(mock, "community", communityValues...)

	cs := &ChatService{DB: db}
	result, err := cs.Chat("How many communities are in the list?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "I found 2 distinct communities in the matching data." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "database_distinct_count" {
		t.Fatalf("expected database distinct-count route, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersGroupSummaryFromDatabase(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	communityValues := []databaseDimensionValue{
		{Normalized: "garden river", Display: "Garden River", Count: 3},
		{Normalized: "batchawana bay", Display: "Batchawana Bay", Count: 1},
	}
	expectDatabaseDimensionValues(mock, "community", communityValues...)

	cs := &ChatService{DB: db}
	result, err := cs.Chat("How many students are there per community?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "By community, the matching records break down as Garden River (3) and Batchawana Bay (1)." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "database_group_summary" || result.Debug.RowsSelected != 4 {
		t.Fatalf("expected database group-summary route, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersGroupExtremeWithNamesFromDatabase(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	communityValues := []databaseDimensionValue{
		{Normalized: "garden river", Display: "Garden River", Count: 2},
		{Normalized: "batchawana bay", Display: "Batchawana Bay", Count: 1},
	}
	expectDatabaseDimensionValues(mock, "community", communityValues...)
	expectDatabaseNamedRecords(mock,
		databaseNamedRecord{SourceRowID: 1, DisplayName: "Alice Johnson"},
		databaseNamedRecord{SourceRowID: 2, DisplayName: "Bob Thomas"},
	)

	cs := &ChatService{DB: db}
	result, err := cs.Chat("Which community has the most students, and what are their names?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "Garden River has the highest") || !strings.Contains(result.Answer, "Alice Johnson and Bob Thomas") {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "database_group_extreme" {
		t.Fatalf("expected database group-extreme route, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersCommunityImpactFromDatabaseUsingDatasetDescription(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	const filename = "Shingwauk And Wawanosh Students Master List"
	const description = "Master list of residential school survivors impacted by Shingwauk Residential School, including Wawanosh records."
	expectChatFileLookup(mock, filename, description)
	expectDatabaseDimensionValues(mock, "community",
		databaseDimensionValue{Normalized: "garden river", Display: "Garden River", Count: 3},
		databaseDimensionValue{Normalized: "batchawana bay", Display: "Batchawana Bay", Count: 1},
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if model != routedAnswerFastModel {
			t.Fatalf("model = %q want %q", model, routedAnswerFastModel)
		}
		payload := routedAnswerPayloadFromPrompt(t, contents)
		if payload.DatasetTitle != filename || payload.DatasetDescription != description {
			t.Fatalf("missing dataset scope in routed prompt: %#v", payload)
		}
		if !strings.Contains(payload.VerifiedQueryResult, "Garden River") || !strings.Contains(payload.VerifiedQueryResult, "3") {
			t.Fatalf("missing verified group result: %#v", payload)
		}
		// Even if the model ignores the plain-text instruction and returns the
		// standard structured envelope, only its answer text may reach the UI.
		return rawAnswer(`{"answer":"Garden River has the largest number of listed Shingwauk survivors, with 3 individuals.","matched_row_ref":null}`), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("What community is more impacted?", nil, filename, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "Garden River has the largest number of listed Shingwauk survivors, with 3 individuals." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if strings.HasPrefix(strings.TrimSpace(result.Answer), "{") {
		t.Fatalf("JSON leaked into answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "database_group_extreme" || result.Debug.ExecutionMode != "database_llm" {
		t.Fatalf("expected database impact route with LLM answer, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersFilteredRecordListFromDatabase(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectDatabaseDimensionValues(mock, "community",
		databaseDimensionValue{Normalized: "garden river", Display: "Garden River", Count: 2},
	)
	expectDatabaseDimensionValues(mock, "school")
	expectDatabaseNamedRecords(mock,
		databaseNamedRecord{SourceRowID: 1, DisplayName: "Alice Johnson"},
		databaseNamedRecord{SourceRowID: 2, DisplayName: "Bob Thomas"},
	)

	cs := &ChatService{DB: db}
	result, err := cs.Chat("List all students from Garden River", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "I found 2 matching records: Alice Johnson and Bob Thomas." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "database_record_list" || result.Debug.RowsSelected != 2 {
		t.Fatalf("expected database record-list route, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_CachesFastAnswerByFileVersionAndQuestion(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectDatabaseDimensionValues(mock, "community",
		databaseDimensionValue{Normalized: "garden river", Display: "Garden River", Count: 2},
	)
	expectDatabaseDimensionValues(mock, "school")
	expectDatabaseCount(mock, 2)
	expectChatFileLookup(mock, "sheet.xlsx")

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	modelCalls := 0
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		modelCalls++
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	first, err := cs.Chat("How many students are from Garden River?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("first chat: %v", err)
	}
	second, err := cs.Chat("How many students are from Garden River?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("second chat: %v", err)
	}
	if first.Answer != second.Answer {
		t.Fatalf("cached answer mismatch: first=%q second=%q", first.Answer, second.Answer)
	}
	if modelCalls != 1 {
		t.Fatalf("routed answer model calls = %d want 1", modelCalls)
	}
	if second.Debug == nil || second.Debug.Strategy != "fast_answer_cache" || second.Debug.ExecutionMode != "cache" {
		t.Fatalf("expected versioned fast-answer cache, got %#v", second.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_TryCoalescedFastTextChat_SharesDatabaseAndRendererWork(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectDatabaseDimensionValues(mock, "community",
		databaseDimensionValue{Normalized: "garden river", Display: "Garden River", Count: 2},
	)
	expectDatabaseDimensionValues(mock, "school")
	expectDatabaseCount(mock, 2)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	var modelCalls atomic.Int32
	modelStarted := make(chan struct{})
	releaseModel := make(chan struct{})
	var startedOnce sync.Once
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		modelCalls.Add(1)
		startedOnce.Do(func() { close(modelStarted) })
		<-releaseModel
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	input := ChatQueryInput{FileID: 1, Version: 1, FileName: "sheet.xlsx", Question: "How many students are from Garden River?"}
	const requestCount = 8
	results := make([]*ChatResult, requestCount)
	errs := make([]error, requestCount)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results[index], _, errs[index] = cs.tryCoalescedFastTextChat(input)
		}(i)
	}
	close(start)
	select {
	case <-modelStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for routed renderer")
	}
	// Give the other goroutines time to join the in-flight request.
	time.Sleep(20 * time.Millisecond)
	close(releaseModel)
	wg.Wait()

	if modelCalls.Load() != 1 {
		t.Fatalf("model calls = %d want 1", modelCalls.Load())
	}
	for index := range results {
		if errs[index] != nil || results[index] == nil || !strings.Contains(results[index].Answer, "2") {
			t.Fatalf("request %d result=%#v err=%v", index, results[index], errs[index])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_GenerateCoalescedFullTextAnswer_SharesModelRequest(t *testing.T) {
	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	var modelCalls atomic.Int32
	modelStarted := make(chan struct{})
	releaseModel := make(chan struct{})
	var startedOnce sync.Once
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		modelCalls.Add(1)
		startedOnce.Do(func() { close(modelStarted) })
		<-releaseModel
		return rawAnswer("Shared answer."), nil
	}

	cs := &ChatService{Client: &genai.Client{}}
	input := ChatQueryInput{FileID: 1, Version: 1, Question: "Explain the notes"}
	const requestCount = 8
	answers := make([]string, requestCount)
	errs := make([]error, requestCount)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			answers[index], _, errs[index] = cs.generateCoalescedFullTextAnswer(context.Background(), input, "prompt", chatFastModel, chatQualityModel)
		}(i)
	}
	close(start)
	select {
	case <-modelStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for full renderer")
	}
	time.Sleep(20 * time.Millisecond)
	close(releaseModel)
	wg.Wait()

	if modelCalls.Load() != 1 {
		t.Fatalf("model calls = %d want 1", modelCalls.Load())
	}
	for index := range answers {
		if errs[index] != nil || answers[index] != "Shared answer." {
			t.Fatalf("request %d answer=%q err=%v", index, answers[index], errs[index])
		}
	}
}

func TestRoutedAnswerLimitsScaleWithoutTruncatingLists(t *testing.T) {
	if got := routedAnswerOutputTokenLimit("Two records."); got != minRoutedAnswerOutputTokens {
		t.Fatalf("short answer token limit = %d want %d", got, minRoutedAnswerOutputTokens)
	}
	medium := strings.Repeat("Community Name, ", 200)
	if got := routedAnswerOutputTokenLimit(medium); got <= minRoutedAnswerOutputTokens || got >= maxRoutedAnswerOutputTokens {
		t.Fatalf("medium answer token limit = %d", got)
	}
	if got := routedAnswerOutputTokenLimit(strings.Repeat("Community Name, ", 2000)); got != maxRoutedAnswerOutputTokens {
		t.Fatalf("large answer token limit = %d want %d", got, maxRoutedAnswerOutputTokens)
	}
	if got := routedAnswerGenerationTimeout("Two records."); got != minRoutedAnswerTimeout {
		t.Fatalf("short answer timeout = %s want %s", got, minRoutedAnswerTimeout)
	}
	if got := routedAnswerGenerationTimeout(strings.Repeat("x", 10000)); got != maxRoutedAnswerTimeout {
		t.Fatalf("large answer timeout = %s want %s", got, maxRoutedAnswerTimeout)
	}
}

func TestChatService_Chat_CachesRepeatedTextModelAnswer(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{"name": "Alice", "has_notes": true},
			withCanonical(map[string]any{"display_name": "Alice"}),
			withNarrative(map[string]any{"notes": "Alice appears in the register."}),
		),
	)
	expectChatFileLookup(mock, "sheet.xlsx")

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	calls := 0
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		calls++
		return jsonStructuredAnswer("Alice appears in the register.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	question := "Explain the notes for Alice in a warm paragraph"
	first, err := cs.Chat(question, nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("first chat: %v", err)
	}
	second, err := cs.Chat(question, nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("second chat: %v", err)
	}
	if calls != 1 {
		t.Fatalf("model calls = %d want 1", calls)
	}
	if first.Answer != second.Answer || second.Debug == nil || second.Debug.ExecutionMode != "cache" || second.Debug.UsedModel != "" {
		t.Fatalf("expected cached text-model answer, first=%#v second=%#v", first, second)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_FinalizeRoutedChatAnswer_RetainsVerifiedResultOnModelFailure(t *testing.T) {
	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return nil, errors.New("temporary model failure")
	}

	result := &ChatResult{
		Answer: "The communities are Garden River and Walpole Island.",
		Debug: &ChatDebugMetrics{
			ExecutionMode: "database",
		},
	}
	cs := &ChatService{Client: &genai.Client{}}

	finalized := cs.finalizeRoutedChatAnswer(context.Background(), ChatQueryInput{Question: "List every community"}, result)
	if finalized.Answer != "The communities are Garden River and Walpole Island." {
		t.Fatalf("verified fallback answer changed: %q", finalized.Answer)
	}
	if finalized.Debug == nil || finalized.Debug.ExecutionMode != "database_fallback" || finalized.Debug.PromptBytes == 0 {
		t.Fatalf("expected routed model fallback diagnostics, got %#v", finalized.Debug)
	}
	input := ChatQueryInput{FileID: 1, Version: 1, Question: "List every community"}
	cs.storeFastChatAnswer(input, finalized)
	if _, ok := cs.getFastChatAnswer(input); ok {
		t.Fatal("model-fallback answer must not prevent the next request from retrying generation")
	}
}

func TestChatService_FinalizeRoutedChatAnswer_FallsBackWhenLiteModelIsUnavailable(t *testing.T) {
	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})
	models := make([]string, 0, 2)
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		models = append(models, model)
		if model == routedAnswerFastModel {
			return nil, errors.New("model unavailable")
		}
		return rawAnswer("Garden River has the largest number of listed survivors."), nil
	}

	result := &ChatResult{
		Answer: "Garden River has the highest number of matching records.",
		Debug:  &ChatDebugMetrics{ExecutionMode: "database"},
	}
	cs := &ChatService{Client: &genai.Client{}}
	finalized := cs.finalizeRoutedChatAnswer(context.Background(), ChatQueryInput{Question: "Which community is most impacted?"}, result)

	if finalized.Answer != "Garden River has the largest number of listed survivors." {
		t.Fatalf("unexpected fallback-model answer: %q", finalized.Answer)
	}
	if finalized.Debug == nil || finalized.Debug.ExecutionMode != "database_llm" || finalized.Debug.UsedModel != chatFastModel {
		t.Fatalf("unexpected fallback-model diagnostics: %#v", finalized.Debug)
	}
	if len(models) != 2 || models[0] != routedAnswerFastModel || models[1] != chatFastModel {
		t.Fatalf("model sequence = %#v", models)
	}
}

func TestFastChatAnswerCacheKey_ChangesWithDatasetDescription(t *testing.T) {
	base := ChatQueryInput{FileID: 1, Version: 1, Question: "Which community is most impacted?"}
	withDescription := base
	withDescription.FileDescription = "Master list of Shingwauk residential school survivors."
	if fastChatAnswerCacheKey(base) == fastChatAnswerCacheKey(withDescription) {
		t.Fatal("dataset-description change must invalidate cached wording")
	}
}

func TestChatService_Chat_AnswersDatasetOverviewFromDatabase(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectDatabaseCount(mock, 5)
	expectDatabaseDimensionValues(mock, "community",
		databaseDimensionValue{Normalized: "garden river", Display: "Garden River", Count: 3},
		databaseDimensionValue{Normalized: "batchawana bay", Display: "Batchawana Bay", Count: 2},
	)
	expectDatabaseDimensionValues(mock, "school",
		databaseDimensionValue{Normalized: "shingwauk", Display: "Shingwauk", Count: 5},
	)

	cs := &ChatService{DB: db}
	result, err := cs.Chat("Give me an overview of the dataset", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "5 records across 2 distinct communities and 1 distinct school") ||
		!strings.Contains(result.Answer, "Garden River (3)") ||
		!strings.Contains(result.Answer, "Shingwauk (5)") {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "database_dataset_overview" || result.Debug.RowsSelected != 5 {
		t.Fatalf("expected database dataset-overview route, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatService_Chat_AnswersFieldCatalogFromDataConfig(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	mock.ExpectQuery(`(?i)select.*config.*from.*data_config.*where.*file_id.*is_active.*order.*updated_at.*id.*limit`).
		WithArgs(int64(1), true, 1).
		WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow(`{
			"source_file": {
				"data_config": {
					"fields": [
						{"key":"student_name","label":"Student Name"},
						{"key":"community","label":"Community"},
						{"key":"first_language","label":"First Language"}
					]
				}
			}
		}`))

	cs := &ChatService{DB: db}
	result, err := cs.Chat("What fields are available?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Answer != "The 3 configured fields are Community, First Language, and Student Name." {
		t.Fatalf("unexpected answer: %q", result.Answer)
	}
	if result.Debug == nil || result.Debug.RetrievalMode != "database_field_catalog" {
		t.Fatalf("expected database field-catalog route, got %#v", result.Debug)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPlanDatabaseChatQuestion_CoversCommonCheapQuestions(t *testing.T) {
	tests := []struct {
		question  string
		operation databaseChatOperation
	}{
		{question: "Show students in Garden River", operation: databaseOperationRecordList},
		{question: "Who is from Garden River?", operation: databaseOperationRecordList},
		{question: "Compare the communities", operation: databaseOperationGroupSummary},
		{question: "Give me a breakdown by school", operation: databaseOperationGroupSummary},
		{question: "What are the top communities?", operation: databaseOperationGroupExtreme},
		{question: "What community is more impacted?", operation: databaseOperationGroupExtreme},
		{question: "Which community has highest amount of individuals?", operation: databaseOperationGroupExtreme},
		{question: "What is the total number of records?", operation: databaseOperationCount},
		{question: "Does Garden River have any students?", operation: databaseOperationExistence},
		{question: "What columns are available?", operation: databaseOperationFieldCatalog},
		{question: "Summarize dataset", operation: databaseOperationDatasetView},
	}

	for _, test := range tests {
		t.Run(test.question, func(t *testing.T) {
			plan, ok := planDatabaseChatQuestion(test.question)
			if !ok {
				t.Fatalf("expected database plan for %q", test.question)
			}
			if plan.Operation != test.operation {
				t.Fatalf("operation = %q want %q", plan.Operation, test.operation)
			}
		})
	}
}

func TestDatabasePlanSupportsCommunityImpactPhrases(t *testing.T) {
	questions := []string{
		"What community is more impacted?",
		"Which community has highest amount of individuals?",
		"Which community has the maximum number of people?",
	}

	for _, question := range questions {
		t.Run(question, func(t *testing.T) {
			plan, ok := planDatabaseChatQuestion(question)
			if !ok || plan.Operation != databaseOperationGroupExtreme || plan.Field != "community" || plan.Extreme != "highest" {
				t.Fatalf("unexpected database plan: %#v, ok=%v", plan, ok)
			}
			ctx := buildDeterministicQuestionContext(nil, question)
			if !databasePlanSupportsContext(plan, ctx) {
				t.Fatalf("expected database plan to accept context: %#v", ctx)
			}
		})
	}
}

func TestChatService_Chat_FallsBackToLLMForUnsupportedDeterministicCount(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name": "Alice Johnson",
			},
			withCanonical(map[string]any{
				"display_name": "Alice Johnson",
			}),
			withNarrative(map[string]any{
				"notes": "Drowning was mentioned in the school correspondence.",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return jsonStructuredAnswer("Yes. One matched record mentions drowning.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("How many students died from drowning?", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil {
		t.Fatal("expected debug metrics")
	}
	if result.Debug.ExecutionMode != "llm" {
		t.Fatalf("execution mode = %q want llm", result.Debug.ExecutionMode)
	}
}

func TestChatService_Chat_UsesAllDeathRowsForAggregateYearQuestion(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	filename := "Shingwauk And Wawanosh Students Master List"
	expectChatFileLookup(mock, filename)
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":              "Alice Johnson",
				"community":         "Garden River",
				"deceased_status":   "Yes",
				"has_death_details": true,
			},
			withCanonical(map[string]any{
				"display_name":    "Alice Johnson",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
			withNarrative(map[string]any{
				"notes":         "Referenced in the Shingwauk register.",
				"death_details": "Died 13 Jan 1913, obituary.",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":              "Bob Thomas",
				"community":         "Batchawana Bay",
				"deceased_status":   "Yes",
				"has_death_details": true,
			},
			withCanonical(map[string]any{
				"display_name":    "Bob Thomas",
				"community":       "Batchawana Bay",
				"deceased_status": "yes",
			}),
			withNarrative(map[string]any{
				"death_details": "Died 2 Dec 1913, grave marker.",
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":              "Carol Pine",
				"community":         "Garden River",
				"deceased_status":   "Yes",
				"has_death_details": true,
			},
			withCanonical(map[string]any{
				"display_name":    "Carol Pine",
				"community":       "Garden River",
				"deceased_status": "yes",
			}),
			withNarrative(map[string]any{
				"death_details": "Died 4 Mar 1914, church record.",
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		prompt := contents[0].Parts[0].Text
		if !strings.Contains(prompt, "Died 13 Jan 1913, obituary.") || !strings.Contains(prompt, "Died 2 Dec 1913, grave marker.") || !strings.Contains(prompt, "Died 4 Mar 1914, church record.") {
			t.Fatalf("expected all death rows in prompt, got:\n%s", prompt)
		}
		return jsonStructuredAnswer("The highest number of recorded deaths occurred in 1913.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("In what years did the highest number of deaths occur at Shingwauk?", nil, filename, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil {
		t.Fatal("expected debug metrics")
	}
	if result.Debug.RetrievalMode != "aggregate_dataset" {
		t.Fatalf("expected aggregate_dataset retrieval for broad death-year question, got %#v", result.Debug)
	}
	if result.Debug.RowsSelected != 3 {
		t.Fatalf("expected all death rows to be selected, got %#v", result.Debug)
	}
}

func TestChatService_Chat_RoutesDeathYearExtremeDeterministicallyForDeathDataset(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	filename := "Confirmed- Shingwauk (Wawanosh)"
	expectChatFileLookup(mock, filename)
	expectChatNormalizedRowsLookup(
		mock,
		normalizedChatRowJSON(
			map[string]any{
				"name":          "Freida Augustine",
				"school":        "Shingwauk, Sault Ste. Marie, ON",
				"date_of_death": "1913-04-17",
			},
			withCanonical(map[string]any{
				"display_name": "Freida Augustine",
				"school":       "Shingwauk, Sault Ste. Marie, ON",
				"dates": map[string]any{
					"death": map[string]any{
						"raw":  "1913-04-17",
						"iso":  "1913-04-17",
						"year": 1913,
					},
				},
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":          "Lena Paibomsai",
				"school":        "Shingwauk, Sault Ste. Marie, ON",
				"date_of_death": "1913-05-01",
			},
			withCanonical(map[string]any{
				"display_name": "Lena Paibomsai",
				"school":       "Shingwauk, Sault Ste. Marie, ON",
				"dates": map[string]any{
					"death": map[string]any{
						"raw":  "1913-05-01",
						"iso":  "1913-05-01",
						"year": 1913,
					},
				},
			}),
		),
		normalizedChatRowJSON(
			map[string]any{
				"name":          "Andrew Johnson",
				"school":        "Shingwauk, Sault Ste. Marie, ON",
				"date_of_death": "1906-03-07",
			},
			withCanonical(map[string]any{
				"display_name": "Andrew Johnson",
				"school":       "Shingwauk, Sault Ste. Marie, ON",
				"dates": map[string]any{
					"death": map[string]any{
						"raw":  "1906-03-07",
						"iso":  "1906-03-07",
						"year": 1906,
					},
				},
			}),
		),
	)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return verifiedRoutedAnswer(t, contents), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("In what years did the highest number of deaths occur at Shingwauk?", nil, filename, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(result.Answer, "1913") || !strings.Contains(result.Answer, "2") {
		t.Fatalf("unexpected answer: %#v", result)
	}
	if result.Debug == nil || result.Debug.QueryType != "death_year_extreme" {
		t.Fatalf("expected deterministic death_year_extreme debug, got %#v", result.Debug)
	}
}

func TestChatService_Chat_UsesFlashPrimaryForBroadCompactPrompt(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	expectChatFileLookup(mock, "sheet.xlsx")
	rows := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		rows = append(rows, normalizedChatRowJSON(
			map[string]any{
				"name":      "Student " + strconv.Itoa(i+1),
				"community": "Garden River",
				"school":    "Shingwauk",
			},
			withCanonical(map[string]any{
				"display_name": "Student " + strconv.Itoa(i+1),
				"community":    "Garden River",
				"school":       "Shingwauk",
			}),
		))
	}
	expectChatNormalizedRowsLookup(mock, rows...)

	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() {
		genaiGenerateContentHook = oldGenerate
	})

	calls := 0
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		calls++
		if calls == 1 && model != "gemini-2.5-flash" {
			t.Fatalf("expected flash as primary model for broad compact prompt, got %q", model)
		}
		return jsonStructuredAnswer("I found 20 matching student records.", nil), nil
	}

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	result, err := cs.Chat("Show the records", nil, "sheet.xlsx", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Debug == nil {
		t.Fatal("expected debug metrics")
	}
	if result.Debug.PrimaryModel != "gemini-2.5-flash" {
		t.Fatalf("primary model = %q want gemini-2.5-flash", result.Debug.PrimaryModel)
	}
	if result.Debug.UsedModel != "gemini-2.5-flash" {
		t.Fatalf("used model = %q want gemini-2.5-flash", result.Debug.UsedModel)
	}
}

func TestDescribeRow_RowNotFound(t *testing.T) {
	db, mock, cleanup := newMockDBChatSvc(t)
	defer cleanup()

	mock.ExpectQuery(`(?i)select.*from.*file_data.*where.*id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "file_id", "row_data", "inserted_by", "created_at", "updated_at", "version"}))

	cs := &ChatService{DB: db, Client: &genai.Client{}}
	_, err := cs.DescribeRow(99)
	if err == nil || !strings.Contains(err.Error(), "row not found") {
		t.Fatalf("expected row not found, got %v", err)
	}
}

func TestGenerateWith429Fallback_UsesFallbackModel(t *testing.T) {
	old := genaiGenerateContentHook
	defer func() { genaiGenerateContentHook = old }()

	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, _ []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if config != nil {
			t.Fatalf("expected nil config, got %#v", config)
		}
		if model == "gemini-2.5-flash" {
			return nil, &googleapi.Error{Code: http.StatusTooManyRequests}
		}
		return rawAnswer("fallback ok"), nil
	}

	cs := &ChatService{Client: &genai.Client{}}
	resp, model, err := cs.generateWith429Fallback(context.Background(), []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if model != "gemini-2.5-pro" {
		t.Fatalf("model = %q want gemini-2.5-pro", model)
	}
	if firstTextFromResp(resp) != "fallback ok" {
		t.Fatalf("unexpected response: %#v", resp)
	}
}
