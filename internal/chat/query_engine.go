package chat

import (
	"encoding/json"
	"fmt"
	"mime/multipart"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	f "nordik-drive-api/internal/file"
	"nordik-drive-api/internal/requestscope"
)

type chatDatasetCacheEntry struct {
	rows     []cachedChatRow
	prepared sync.Map
}

type cachedChatRow struct {
	RowID     int
	RowJSON   string
	Community string
}

type preparedChatDataset struct {
	PromptJSON   string
	RowRefToID   map[string]int
	TotalRows    int
	SelectedRows int
}

type chatStructuredResponse struct {
	Answer        string  `json:"answer"`
	MatchedRowRef *string `json:"matched_row_ref"`
}

const chatStyleInstruction = `
You are a helpful assistant answering a community data question for a non-technical user.

Style requirements:
- Answer like a human: natural, warm, and conversational.
- Prefer short paragraphs over bullet points.
- Do NOT use bullet points unless the user explicitly asks for a list.
- Do NOT sound robotic or overly formal.
- Do NOT mention JSON, database, columns, file name, file version, or any technical details.
- If the answer is not present in the provided data, say so clearly and ask 1 short follow-up question if needed.

Accuracy requirements:
- Use ONLY the provided data below. Do not use outside knowledge.
- Carefully analyze ALL provided data before answering.
- Do not stop after finding the first match.
- Always check if multiple equally correct answers exist.
- Never return only one answer if multiple valid answers are present.
- For questions involving highest, lowest, most, least, first, last, top, or similar comparisons:
  verify that no other entries share the same value before answering.

Before responding:
- Internally re-check the data to confirm whether another valid answer exists.
- Only respond after confirming that all correct answers are included.

Answer format:
- Start with a direct answer in 1-2 sentences.
- If multiple answers exist, include ALL of them in the first sentence.
- Combine them naturally (example: "1882 and 1995").
- Provide as much detail as possible based on the data.
`

const chatStructuredOutputInstruction = `
Return ONLY a single JSON object with this shape:
{
  "answer": "your natural-language answer",
  "matched_row_ref": "R1" or null
}

Rules for matched_row_ref:
- Set it only when the user's question is about a person and exactly one row is the best match.
- You may also set it when your answer is based entirely on one single row, even if the question is not explicitly about a person.
- If the answer depends on multiple rows, or more than one row could match, set matched_row_ref to null.
- If a person's name appears misspelled but the intended single row is still clear from the data, you may still choose that row_ref.
- Use only row_ref values that appear in the DATA.
- Never invent, guess, or approximate a row_ref.
`

func (cs *ChatService) Chat(question string, audioFile *multipart.FileHeader, filename string, communities []string) (*ChatResult, error) {
	totalStart := time.Now()
	if cs.DB == nil {
		return nil, fmt.Errorf("db not initialized")
	}

	file, err := cs.lookupChatFile(filename)
	if err != nil {
		return nil, err
	}

	input := ChatQueryInput{
		FileID:          file.ID,
		Version:         file.Version,
		FileName:        filename,
		FileDescription: file.Description,
		Question:        question,
		Communities:     communities,
	}

	if audioFile == nil {
		// Use the cheapest reliable retrieval path first: cached answer, direct
		// database query, then the broader in-memory deterministic router. A
		// compact LLM pass turns verified routed results into the final response;
		// only questions these paths cannot prove use full structured retrieval.
		routed, ok, err := cs.tryCoalescedFastTextChat(input)
		if err != nil {
			return nil, err
		}
		if ok {
			if routed.Debug != nil {
				routed.Debug.TotalMillis = time.Since(totalStart).Milliseconds()
			}
			return routed, nil
		}
	}
	if cs.Client == nil {
		return nil, fmt.Errorf("genai client not initialized")
	}

	strategy := cs.getQueryStrategy()
	prepared, err := strategy.Prepare(cs, ChatQueryInput{
		FileID:          input.FileID,
		Version:         input.Version,
		FileName:        input.FileName,
		FileDescription: input.FileDescription,
		Question:        input.Question,
		Communities:     input.Communities,
	})
	if err != nil {
		return nil, err
	}

	ctx := requestscope.Context(cs.DB)

	var audioBytes []byte
	var audioMime string
	if audioFile != nil {
		fh, err := openMultipartFileHook(audioFile)
		if err != nil {
			return nil, fmt.Errorf("failed to open audio file: %w", err)
		}
		defer fh.Close()

		audioBytes, err = readAllHook(fh)
		if err != nil {
			return nil, fmt.Errorf("failed to read audio file: %w", err)
		}

		audioMime = audioFile.Header.Get("Content-Type")
	}

	primaryModel, fallbackModel := selectChatModelPlan(prepared.Debug, len(audioBytes) > 0)
	generationStart := time.Now()
	var answer string
	var usedModel string
	if len(audioBytes) == 0 {
		answer, usedModel, err = cs.generateCoalescedFullTextAnswer(ctx, input, prepared.Prompt, primaryModel, fallbackModel)
	} else {
		answer, usedModel, err = cs.generateFromPromptWithModels(ctx, prepared.Prompt, audioBytes, audioMime, primaryModel, fallbackModel)
	}
	if err != nil {
		return nil, fmt.Errorf("generation error (%s): %w", usedModel, err)
	}

	resolvedAnswer, matchedRowID := resolveChatResponse(answer, prepared.RowRefToID)
	debug := prepared.Debug
	debug.ExecutionMode = "llm"
	debug.AudioIncluded = len(audioBytes) > 0
	debug.PrimaryModel = primaryModel
	debug.UsedModel = usedModel
	debug.GenerationMillis = time.Since(generationStart).Milliseconds()
	debug.TotalMillis = time.Since(totalStart).Milliseconds()

	result := &ChatResult{
		Answer:       resolvedAnswer,
		MatchedRowID: matchedRowID,
		Debug:        &debug,
	}
	if len(audioBytes) == 0 {
		cs.storeFastChatAnswer(input, result)
	}
	return result, nil
}

func (cs *ChatService) ChatForUser(_ int64, question string, audioFile *multipart.FileHeader, filename string, communities []string) (*ChatResult, error) {
	return cs.Chat(question, audioFile, filename, communities)
}

func chatDatasetCacheKey(fileID uint, version int) string {
	return fmt.Sprintf("%d:%d", fileID, version)
}

func chatCommunitiesCacheKey(communities []string) string {
	if len(communities) == 0 {
		return "__all__"
	}
	return strings.Join(communities, "\x1f")
}

func (cs *ChatService) getPreparedChatDataset(fileID uint, version int, communities []string) (*preparedChatDataset, error) {
	dataset, err := cs.getOrLoadChatDataset(fileID, version)
	if err != nil {
		return nil, err
	}

	cacheKey := chatCommunitiesCacheKey(communities)
	if cached, ok := dataset.prepared.Load(cacheKey); ok {
		if prepared, ok := cached.(*preparedChatDataset); ok {
			return prepared, nil
		}
	}

	prepared, err := buildPreparedChatDataset(dataset.rows, communities)
	if err != nil {
		return nil, err
	}

	actual, _ := dataset.prepared.LoadOrStore(cacheKey, prepared)
	if cached, ok := actual.(*preparedChatDataset); ok {
		return cached, nil
	}

	return prepared, nil
}

func (cs *ChatService) getOrLoadChatDataset(fileID uint, version int) (*chatDatasetCacheEntry, error) {
	cacheKey := chatDatasetCacheKey(fileID, version)
	if cached, ok := cs.sharedState().datasetCache.load(cacheKey); ok {
		if entry, ok := cached.(*chatDatasetCacheEntry); ok {
			return entry, nil
		}
	}

	var rawRows []f.FileData
	if err := cs.DB.Select("id, row_data").
		Where("file_id = ? AND version = ?", fileID, version).
		Order("id ASC").
		Find(&rawRows).Error; err != nil {
		return nil, fmt.Errorf("file data not found: %w", err)
	}

	rows := make([]cachedChatRow, 0, len(rawRows))
	for _, rawRow := range rawRows {
		rows = append(rows, buildCachedChatRow(rawRow))
	}

	entry := &chatDatasetCacheEntry{rows: rows}
	actual := cs.sharedState().datasetCache.loadOrStore(cacheKey, entry)
	if cached, ok := actual.(*chatDatasetCacheEntry); ok {
		return cached, nil
	}

	return entry, nil
}

func buildCachedChatRow(rawRow f.FileData) cachedChatRow {
	row := cachedChatRow{
		RowID:   int(rawRow.ID),
		RowJSON: string(rawRow.RowData),
	}

	var rowMap map[string]interface{}
	if err := json.Unmarshal(rawRow.RowData, &rowMap); err != nil {
		return row
	}

	row.Community = extractCommunityValue(rowMap)
	return row
}

func extractCommunityValue(rowMap map[string]interface{}) string {
	rawVal, ok := rowMap["First Nation/Community"]
	if !ok || rawVal == nil {
		return ""
	}

	valStr, ok := rawVal.(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(valStr)
}

func buildPreparedChatDataset(rows []cachedChatRow, communities []string) (*preparedChatDataset, error) {
	selectedIndexes := make([]int, 0, len(rows))
	if len(communities) == 0 {
		for idx := range rows {
			selectedIndexes = append(selectedIndexes, idx)
		}
	} else {
		allowed := make(map[string]struct{}, len(communities))
		for _, community := range communities {
			allowed[community] = struct{}{}
		}
		for idx, row := range rows {
			if _, ok := allowed[row.Community]; ok {
				selectedIndexes = append(selectedIndexes, idx)
			}
		}
	}

	promptJSON, rowRefToID, err := buildPromptJSONArray(rows, selectedIndexes)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal file data: %w", err)
	}

	return &preparedChatDataset{
		PromptJSON:   promptJSON,
		RowRefToID:   rowRefToID,
		TotalRows:    len(rows),
		SelectedRows: len(selectedIndexes),
	}, nil
}

func buildPromptJSONArray(rows []cachedChatRow, indexes []int) (string, map[string]int, error) {
	if len(indexes) == 0 {
		return "[]", map[string]int{}, nil
	}

	var b strings.Builder
	rowRefToID := make(map[string]int, len(indexes))
	b.WriteByte('[')
	for idx, rowIndex := range indexes {
		rowJSON := rows[rowIndex].RowJSON
		if !json.Valid([]byte(rowJSON)) {
			return "", nil, fmt.Errorf("invalid row json")
		}
		if idx > 0 {
			b.WriteByte(',')
		}
		rowRef := buildPromptRowRef(idx + 1)
		rowRefToID[rowRef] = rows[rowIndex].RowID
		b.WriteString(`{"row_ref":`)
		b.WriteString(strconv.Quote(rowRef))
		b.WriteString(`,"row_data":`)
		b.WriteString(rowJSON)
		b.WriteByte('}')
	}
	b.WriteByte(']')
	return b.String(), rowRefToID, nil
}

func buildPromptRowRef(position int) string {
	return fmt.Sprintf("R%d", position)
}

func resolveChatResponse(raw string, rowRefToID map[string]int) (string, *int) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	structured, ok := parseStructuredChatResponse(raw)
	if !ok {
		if answer := extractMalformedStructuredChatAnswer(raw); answer != "" {
			return answer, nil
		}
		if looksLikeJSONChatResponse(raw) {
			return "I'm sorry, I couldn't format that answer correctly. Please try again.", nil
		}
		return raw, nil
	}

	answer := strings.TrimSpace(structured.Answer)
	if answer == "" {
		answer = raw
	}

	matchedRowID := resolveMatchedRowRef(structured.MatchedRowRef, rowRefToID)
	return answer, matchedRowID
}

func looksLikeJSONChatResponse(raw string) bool {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```json") || strings.HasPrefix(text, "```JSON") {
		return true
	}
	return strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[")
}

// extractMalformedStructuredChatAnswer prevents a nearly-correct model JSON
// response from leaking into the UI. It only activates when an answer property
// is followed by the expected matched_row_ref property, and uses the last quote
// before that property so an accidental unescaped quote inside the answer does
// not truncate the recovered text.
func extractMalformedStructuredChatAnswer(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" || (!strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "```")) {
		return ""
	}

	answerKey := strings.Index(text, `"answer"`)
	if answerKey < 0 {
		return ""
	}
	valueText := text[answerKey+len(`"answer"`):]
	colon := strings.IndexByte(valueText, ':')
	if colon < 0 {
		return ""
	}
	valueText = strings.TrimLeft(valueText[colon+1:], " \t\r\n")
	if !strings.HasPrefix(valueText, `"`) {
		return ""
	}
	valueText = valueText[1:]

	matchedKey := strings.Index(valueText, `"matched_row_ref"`)
	if matchedKey < 0 {
		return ""
	}
	beforeMatchedKey := strings.TrimSpace(valueText[:matchedKey])
	beforeMatchedKey = strings.TrimSuffix(beforeMatchedKey, ",")
	beforeMatchedKey = strings.TrimSpace(beforeMatchedKey)
	endQuote := strings.LastIndexByte(beforeMatchedKey, '"')
	if endQuote < 0 {
		return ""
	}

	candidate := strings.TrimSpace(beforeMatchedKey[:endQuote])
	if candidate == "" {
		return ""
	}
	if decoded, err := strconv.Unquote(`"` + candidate + `"`); err == nil {
		return strings.TrimSpace(decoded)
	}

	// The JSON was malformed, so decode the common escapes conservatively while
	// retaining the human-readable answer instead of exposing the whole object.
	candidate = strings.ReplaceAll(candidate, `\n`, "\n")
	candidate = strings.ReplaceAll(candidate, `\r`, "\r")
	candidate = strings.ReplaceAll(candidate, `\t`, "\t")
	candidate = strings.ReplaceAll(candidate, `\"`, `"`)
	candidate = strings.ReplaceAll(candidate, `\\`, `\`)
	return strings.TrimSpace(candidate)
}

func parseStructuredChatResponse(raw string) (*chatStructuredResponse, bool) {
	jsonText := extractJSONObjectCandidate(raw)
	if jsonText == "" {
		return nil, false
	}

	var structured chatStructuredResponse
	if err := json.Unmarshal([]byte(jsonText), &structured); err != nil {
		return nil, false
	}

	structured.Answer = strings.TrimSpace(structured.Answer)
	if structured.Answer == "" {
		return nil, false
	}

	if structured.MatchedRowRef != nil {
		rowRef := normalizePromptRowRef(*structured.MatchedRowRef)
		if rowRef == "" {
			structured.MatchedRowRef = nil
		} else {
			structured.MatchedRowRef = &rowRef
		}
	}

	return &structured, true
}

func extractJSONObjectCandidate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```JSON")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSpace(raw)
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}

	if json.Valid([]byte(raw)) {
		return raw
	}

	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start == -1 || end == -1 || end < start {
		return ""
	}

	candidate := strings.TrimSpace(raw[start : end+1])
	if !json.Valid([]byte(candidate)) {
		return ""
	}

	return candidate
}

func resolveMatchedRowRef(rowRef *string, rowRefToID map[string]int) *int {
	if rowRef == nil {
		return nil
	}

	normalized := normalizePromptRowRef(*rowRef)
	if normalized == "" {
		return nil
	}

	rowID, ok := rowRefToID[normalized]
	if !ok {
		return nil
	}

	resolved := rowID
	return &resolved
}

func normalizePromptRowRef(rowRef string) string {
	rowRef = strings.ToUpper(strings.TrimSpace(rowRef))
	if rowRef == "" {
		return ""
	}
	if !strings.HasPrefix(rowRef, "R") {
		return ""
	}

	for _, r := range rowRef[1:] {
		if !unicode.IsDigit(r) {
			return ""
		}
	}

	return rowRef
}

func isMissingDescriptionColumnError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "description") {
		return false
	}
	return strings.Contains(message, "column") && (strings.Contains(message, "does not exist") || strings.Contains(message, "unknown"))
}
