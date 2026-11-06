package chat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gorm.io/datatypes"

	f "nordik-drive-api/internal/file"
)

type chatStructuredDatasetCacheEntry struct {
	rows []cachedStructuredChatRow
}

type cachedStructuredChatRow struct {
	SourceRowID         int
	CanonicalName       string
	NameAliasText       string
	IdentifierText      string
	CanonicalCommunity  string
	CanonicalSchool     string
	IsDeathRecord       bool
	DeathYear           *int
	CauseOfDeathText    string
	CoreSearchText      string
	SearchText          string
	DefaultBundle       structuredChatDefaultBundle
	NarrativeBundle     structuredChatNarrativeBundle
	SourceFields        []structuredChatSourceField
	DefaultBundleJSON   string
	NarrativeBundleJSON string
	HasNarrative        bool
}

type preparedStructuredChatDataset struct {
	PromptJSON           string
	RowRefToID           map[string]int
	TotalRows            int
	SelectedRows         int
	RowsWithSchool       int
	NarrativeRows        int
	RetrievalMode        string
	PromptProjectionMode string
}

type structuredChatRowDB struct {
	SourceRowID        uint           `gorm:"column:source_row_id"`
	CanonicalName      string         `gorm:"column:canonical_name"`
	CanonicalCommunity string         `gorm:"column:canonical_community"`
	CanonicalSchool    string         `gorm:"column:canonical_school"`
	SearchText         string         `gorm:"column:search_text"`
	RowDataNormalized  datatypes.JSON `gorm:"column:row_data_normalized"`
}

type structuredChatNormalizedPayload struct {
	Fields    map[string]structuredChatFieldPayload `json:"fields,omitempty"`
	Canonical *structuredChatCanonicalPayload       `json:"canonical,omitempty"`
	Chat      *structuredChatBundlePayload          `json:"chat,omitempty"`
}

type structuredChatFieldPayload struct {
	Raw        string   `json:"raw,omitempty"`
	Normalized string   `json:"normalized,omitempty"`
	Tokens     []string `json:"tokens,omitempty"`
	Role       string   `json:"role,omitempty"`
}

type structuredChatCanonicalPayload struct {
	DisplayName      string                       `json:"display_name,omitempty"`
	NameAliases      []string                     `json:"name_aliases,omitempty"`
	StudentNumber    string                       `json:"student_number,omitempty"`
	StudentNumberRaw string                       `json:"student_number_raw,omitempty"`
	Community        string                       `json:"community,omitempty"`
	School           string                       `json:"school,omitempty"`
	DeceasedStatus   string                       `json:"deceased_status,omitempty"`
	CauseOfDeath     string                       `json:"cause_of_death,omitempty"`
	ParentsNames     string                       `json:"parents_names,omitempty"`
	MappingLocation  string                       `json:"mapping_location,omitempty"`
	Dates            structuredChatCanonicalDates `json:"dates,omitempty"`
}

type structuredChatCanonicalDates struct {
	Birth      *structuredChatCanonicalDate `json:"birth,omitempty"`
	Death      *structuredChatCanonicalDate `json:"death,omitempty"`
	Admitted   *structuredChatCanonicalDate `json:"admitted,omitempty"`
	Discharged *structuredChatCanonicalDate `json:"discharged,omitempty"`
}

type structuredChatCanonicalDate struct {
	Raw  string `json:"raw,omitempty"`
	ISO  string `json:"iso,omitempty"`
	Year *int   `json:"year,omitempty"`
}

type structuredChatBundlePayload struct {
	DefaultBundle   map[string]any `json:"default_bundle,omitempty"`
	NarrativeBundle map[string]any `json:"narrative_bundle,omitempty"`
}

type structuredChatDefaultBundle struct {
	RecordProfile            string   `json:"record_profile,omitempty"`
	Name                     string   `json:"name,omitempty"`
	Aliases                  []string `json:"aliases,omitempty"`
	StudentNumber            string   `json:"student_number,omitempty"`
	Community                string   `json:"community,omitempty"`
	School                   string   `json:"school,omitempty"`
	DeceasedStatus           string   `json:"deceased_status,omitempty"`
	DateOfBirth              string   `json:"date_of_birth,omitempty"`
	DateOfDeath              string   `json:"date_of_death,omitempty"`
	Admitted                 string   `json:"admitted,omitempty"`
	Discharged               string   `json:"discharged,omitempty"`
	CauseOfDeath             string   `json:"cause_of_death,omitempty"`
	ParentsNames             string   `json:"parents_names,omitempty"`
	MappingLocation          string   `json:"mapping_location,omitempty"`
	HasNotes                 bool     `json:"has_notes,omitempty"`
	HasAdditionalInformation bool     `json:"has_additional_information,omitempty"`
	HasDeathDetails          bool     `json:"has_death_details,omitempty"`
	HasPhotos                bool     `json:"has_photos,omitempty"`
}

type structuredChatNarrativeBundle struct {
	Notes                 string `json:"notes,omitempty"`
	AdditionalInformation string `json:"additional_information,omitempty"`
	DeathDetails          string `json:"death_details,omitempty"`
}

type structuredChatSourceField struct {
	Name            string
	NormalizedName  string
	Raw             string
	NormalizedValue string
	Role            string
}

const chatCompactDataInstruction = `
- Each item includes a row_ref and a default_bundle with the structured facts most relevant to the question.
- A narrative_bundle is included only when longer notes/details are likely relevant to the question.
- Some rows may also include source_fields containing raw source columns that are relevant to the question.
- Some non-essential fields may be omitted to keep the prompt compact.
- Only rely on the fields that are actually shown in the bundles below.
`

func (cs *ChatService) getPreparedStructuredChatDataset(fileID uint, version int, question string, communities []string) (*preparedStructuredChatDataset, error) {
	dataset, err := cs.getOrLoadStructuredChatDataset(fileID, version)
	if err != nil {
		return nil, err
	}
	if dataset == nil || len(dataset.rows) == 0 {
		return nil, fmt.Errorf("structured dataset unavailable")
	}

	selection := selectStructuredChatRows(dataset.rows, question, communities)
	projection := buildChatPromptProjectionProfile(question, selection.Mode, len(selection.Indexes), selection.IncludeNarrative)
	profile := buildChatQuestionProfile(question)
	promptJSON, rowRefToID, narrativeRows, err := buildStructuredPromptJSONArray(dataset.rows, selection.Indexes, selection.IncludeNarrative, projection, profile)
	if err != nil {
		return nil, err
	}

	return &preparedStructuredChatDataset{
		PromptJSON:           promptJSON,
		RowRefToID:           rowRefToID,
		TotalRows:            len(dataset.rows),
		SelectedRows:         len(selection.Indexes),
		RowsWithSchool:       countStructuredRowsWithCanonicalSchool(dataset.rows, selection.Indexes),
		NarrativeRows:        narrativeRows,
		RetrievalMode:        selection.Mode,
		PromptProjectionMode: projection.Mode,
	}, nil
}

func (cs *ChatService) getOrLoadStructuredChatDataset(fileID uint, version int) (*chatStructuredDatasetCacheEntry, error) {
	cacheKey := chatDatasetCacheKey(fileID, version)
	if cached, ok := cs.sharedState().structuredDatasetCache.load(cacheKey); ok {
		if entry, ok := cached.(*chatStructuredDatasetCacheEntry); ok {
			return entry, nil
		}
	}

	var rawRows []structuredChatRowDB
	err := cs.DB.Table("file_data_normalized").
		Select("source_row_id, canonical_name, canonical_community, canonical_school, search_text, row_data_normalized").
		Where("file_id = ? AND version = ? AND status = ? AND normalization_version = ?", fileID, version, "ready", f.CurrentNormalizationVersion()).
		Order("source_row_id ASC").
		Scan(&rawRows).Error
	if err != nil {
		return nil, err
	}
	if len(rawRows) == 0 {
		return nil, fmt.Errorf("no normalized rows found")
	}

	rows := make([]cachedStructuredChatRow, 0, len(rawRows))
	for _, rawRow := range rawRows {
		row, ok := buildCachedStructuredChatRow(rawRow)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no structured chat rows available")
	}

	entry := &chatStructuredDatasetCacheEntry{rows: rows}
	actual := cs.sharedState().structuredDatasetCache.loadOrStore(cacheKey, entry)
	if cached, ok := actual.(*chatStructuredDatasetCacheEntry); ok {
		return cached, nil
	}
	return entry, nil
}

func buildCachedStructuredChatRow(rawRow structuredChatRowDB) (cachedStructuredChatRow, bool) {
	row := cachedStructuredChatRow{
		SourceRowID:        int(rawRow.SourceRowID),
		CanonicalName:      strings.TrimSpace(rawRow.CanonicalName),
		CanonicalCommunity: strings.TrimSpace(rawRow.CanonicalCommunity),
		CanonicalSchool:    strings.TrimSpace(rawRow.CanonicalSchool),
		SearchText:         normalizeChatSearchValue(rawRow.SearchText),
	}

	var payload structuredChatNormalizedPayload
	if err := json.Unmarshal(rawRow.RowDataNormalized, &payload); err != nil {
		return cachedStructuredChatRow{}, false
	}

	defaultBundle := map[string]any{}
	narrativeBundle := map[string]any{}
	if payload.Chat != nil {
		if payload.Chat.DefaultBundle != nil {
			defaultBundle = payload.Chat.DefaultBundle
		}
		if payload.Chat.NarrativeBundle != nil {
			narrativeBundle = payload.Chat.NarrativeBundle
		}
	}
	if len(defaultBundle) == 0 {
		defaultBundle = buildFallbackDefaultBundle(payload.Canonical)
	}

	defaultBundleJSON, err := json.Marshal(defaultBundle)
	if err != nil {
		return cachedStructuredChatRow{}, false
	}

	row.DefaultBundleJSON = string(defaultBundleJSON)
	_ = json.Unmarshal(defaultBundleJSON, &row.DefaultBundle)
	row.HasNarrative = len(narrativeBundle) > 0
	if row.HasNarrative {
		narrativeBundleJSON, err := json.Marshal(narrativeBundle)
		if err != nil {
			return cachedStructuredChatRow{}, false
		}
		row.NarrativeBundleJSON = string(narrativeBundleJSON)
		_ = json.Unmarshal(narrativeBundleJSON, &row.NarrativeBundle)
	}
	row.SourceFields = buildStructuredSourceFields(payload.Fields)

	if payload.Canonical != nil {
		identifierValues := []string{
			payload.Canonical.StudentNumber,
			payload.Canonical.StudentNumberRaw,
		}
		coreValues := []string{
			payload.Canonical.DisplayName,
			strings.Join(payload.Canonical.NameAliases, " "),
			payload.Canonical.StudentNumber,
			payload.Canonical.StudentNumberRaw,
			payload.Canonical.Community,
			payload.Canonical.School,
			payload.Canonical.DeceasedStatus,
			payload.Canonical.CauseOfDeath,
			payload.Canonical.ParentsNames,
			payload.Canonical.MappingLocation,
			displayStructuredDate(payload.Canonical.Dates.Birth),
			displayStructuredDate(payload.Canonical.Dates.Death),
			displayStructuredDate(payload.Canonical.Dates.Admitted),
			displayStructuredDate(payload.Canonical.Dates.Discharged),
		}
		row.NameAliasText = normalizeChatSearchValue(strings.Join(payload.Canonical.NameAliases, " "))
		row.IdentifierText = normalizeChatSearchValue(strings.Join(identifierValues, " "))
		row.CoreSearchText = normalizeChatSearchValue(strings.Join(coreValues, " "))
		if row.CanonicalName == "" {
			row.CanonicalName = normalizeChatSearchValue(payload.Canonical.DisplayName)
		}
		if row.CanonicalCommunity == "" {
			row.CanonicalCommunity = normalizeChatSearchValue(payload.Canonical.Community)
		}
		if row.CanonicalSchool == "" {
			row.CanonicalSchool = normalizeChatSearchValue(payload.Canonical.School)
		}
		if payload.Canonical.Dates.Death != nil && payload.Canonical.Dates.Death.Year != nil {
			deathYear := *payload.Canonical.Dates.Death.Year
			row.DeathYear = &deathYear
		}
	}
	if row.CanonicalName == "" {
		row.CanonicalName = normalizeChatSearchValue(row.DefaultBundle.Name)
	}
	if row.CanonicalCommunity == "" {
		row.CanonicalCommunity = normalizeChatSearchValue(row.DefaultBundle.Community)
	}
	if row.CanonicalSchool == "" {
		row.CanonicalSchool = normalizeChatSearchValue(row.DefaultBundle.School)
	}
	if row.DeathYear == nil {
		if parsedYear := extractStructuredYear(row.DefaultBundle.DateOfDeath); parsedYear != nil {
			row.DeathYear = parsedYear
		}
	}
	row.CauseOfDeathText = normalizeChatSearchValue(row.DefaultBundle.CauseOfDeath)
	row.IsDeathRecord = row.DeathYear != nil ||
		strings.TrimSpace(row.DefaultBundle.DateOfDeath) != "" ||
		strings.TrimSpace(row.DefaultBundle.CauseOfDeath) != "" ||
		strings.TrimSpace(row.NarrativeBundle.DeathDetails) != "" ||
		normalizeChatSearchValue(row.DefaultBundle.DeceasedStatus) == "yes"

	if row.CoreSearchText == "" {
		row.CoreSearchText = normalizeChatSearchValue(strings.Join(extractBundleStringValues(defaultBundle), " "))
	}
	if row.SearchText == "" {
		row.SearchText = row.CoreSearchText
	}
	if row.CanonicalName == "" && row.CoreSearchText == "" && row.SearchText == "" {
		return cachedStructuredChatRow{}, false
	}
	return row, true
}

func countStructuredRowsWithCanonicalSchool(rows []cachedStructuredChatRow, indexes []int) int {
	count := 0
	for _, index := range indexes {
		if index < 0 || index >= len(rows) {
			continue
		}
		if strings.TrimSpace(rows[index].CanonicalSchool) != "" || strings.TrimSpace(rows[index].DefaultBundle.School) != "" {
			count++
		}
	}
	return count
}

func buildFallbackDefaultBundle(canonical *structuredChatCanonicalPayload) map[string]any {
	if canonical == nil {
		return map[string]any{}
	}

	bundle := map[string]any{}
	setBundleField(bundle, "name", canonical.DisplayName)
	setBundleField(bundle, "student_number", firstNonEmptyString(canonical.StudentNumberRaw, canonical.StudentNumber))
	setBundleField(bundle, "community", canonical.Community)
	setBundleField(bundle, "school", canonical.School)
	setBundleField(bundle, "deceased_status", canonical.DeceasedStatus)
	setBundleField(bundle, "date_of_birth", displayStructuredDate(canonical.Dates.Birth))
	setBundleField(bundle, "date_of_death", displayStructuredDate(canonical.Dates.Death))
	setBundleField(bundle, "admitted", displayStructuredDate(canonical.Dates.Admitted))
	setBundleField(bundle, "discharged", displayStructuredDate(canonical.Dates.Discharged))
	setBundleField(bundle, "cause_of_death", canonical.CauseOfDeath)
	setBundleField(bundle, "parents_names", canonical.ParentsNames)
	setBundleField(bundle, "mapping_location", canonical.MappingLocation)
	if len(canonical.NameAliases) > 0 {
		bundle["aliases"] = append([]string(nil), canonical.NameAliases...)
	}
	return bundle
}

func buildStructuredPromptJSONArray(rows []cachedStructuredChatRow, indexes []int, includeNarrative bool, projection chatPromptProjectionProfile, profile chatQuestionProfile) (string, map[string]int, int, error) {
	if len(indexes) == 0 {
		return "[]", map[string]int{}, 0, nil
	}

	var builder strings.Builder
	rowRefToID := make(map[string]int, len(indexes))
	narrativeRows := 0

	builder.WriteByte('[')
	for idx, rowIndex := range indexes {
		row := rows[rowIndex]
		if idx > 0 {
			builder.WriteByte(',')
		}

		rowRef := buildPromptRowRef(idx + 1)
		rowRefToID[rowRef] = row.SourceRowID

		builder.WriteString(`{"row_ref":`)
		builder.WriteString(strconv.Quote(rowRef))
		builder.WriteString(`,"default_bundle":`)
		defaultBundleJSON, err := marshalProjectedDefaultBundle(row.DefaultBundle, projection)
		if err != nil {
			return "", nil, 0, err
		}
		builder.WriteString(defaultBundleJSON)
		if includeNarrative && row.HasNarrative && row.NarrativeBundleJSON != "" {
			builder.WriteString(`,"narrative_bundle":`)
			narrativeBundleJSON, err := marshalProjectedNarrativeBundle(row.NarrativeBundle, projection)
			if err != nil {
				return "", nil, 0, err
			}
			builder.WriteString(narrativeBundleJSON)
			narrativeRows++
		}
		if sourceFields := buildProjectedSourceFields(row, profile, projection, len(indexes), includeNarrative); len(sourceFields) > 0 {
			sourceFieldsJSON, err := json.Marshal(sourceFields)
			if err != nil {
				return "", nil, 0, err
			}
			builder.WriteString(`,"source_fields":`)
			builder.Write(sourceFieldsJSON)
		}
		builder.WriteByte('}')
	}
	builder.WriteByte(']')

	return builder.String(), rowRefToID, narrativeRows, nil
}

func displayStructuredDate(date *structuredChatCanonicalDate) string {
	if date == nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmptyString(date.Raw, date.ISO))
}

func extractStructuredYear(value string) *int {
	for _, token := range strings.Fields(normalizeChatSearchValue(value)) {
		if len(token) != 4 {
			continue
		}
		year, err := strconv.Atoi(token)
		if err != nil {
			continue
		}
		return &year
	}
	return nil
}

func extractBundleStringValues(bundle map[string]any) []string {
	if len(bundle) == 0 {
		return nil
	}

	keys := make([]string, 0, len(bundle))
	for key := range bundle {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	values := make([]string, 0, len(bundle))
	for _, key := range keys {
		switch v := bundle[key].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				values = append(values, v)
			}
		case []string:
			values = append(values, v...)
		case []any:
			for _, item := range v {
				if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
					values = append(values, text)
				}
			}
		}
	}
	return values
}

func buildStructuredSourceFields(fields map[string]structuredChatFieldPayload) []structuredChatSourceField {
	if len(fields) == 0 {
		return nil
	}

	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]structuredChatSourceField, 0, len(names))
	for _, name := range names {
		field := fields[name]
		raw := strings.TrimSpace(field.Raw)
		if raw == "" {
			continue
		}
		normalizedName := normalizeChatSearchValue(name)
		normalizedValue := strings.TrimSpace(field.Normalized)
		if normalizedValue == "" {
			normalizedValue = normalizeChatSearchValue(raw)
		}
		out = append(out, structuredChatSourceField{
			Name:            name,
			NormalizedName:  normalizedName,
			Raw:             raw,
			NormalizedValue: normalizedValue,
			Role:            strings.TrimSpace(field.Role),
		})
	}
	return out
}

func buildProjectedSourceFields(row cachedStructuredChatRow, profile chatQuestionProfile, projection chatPromptProjectionProfile, selectedRows int, includeNarrative bool) map[string]string {
	if len(row.SourceFields) == 0 {
		return nil
	}

	includeAll := profile.LooksLikeEntity && selectedRows <= 3
	unresolvedTokens := unresolvedSourceFieldTokens(row, profile, includeNarrative)
	out := make(map[string]string)

	for _, field := range row.SourceFields {
		if includeNarrative && isProjectedNarrativeSourceField(field) {
			continue
		}
		if includeAll {
			out[field.Name] = field.Raw
			continue
		}
		if isProjectedSourceFieldRelevant(field, profile, unresolvedTokens) {
			out[field.Name] = field.Raw
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func unresolvedSourceFieldTokens(row cachedStructuredChatRow, profile chatQuestionProfile, includeNarrative bool) []string {
	if len(profile.Tokens) == 0 {
		return nil
	}

	narrativeText := ""
	if includeNarrative {
		narrativeText = normalizeChatSearchValue(strings.Join([]string{
			row.NarrativeBundle.Notes,
			row.NarrativeBundle.AdditionalInformation,
			row.NarrativeBundle.DeathDetails,
		}, " "))
	}

	out := make([]string, 0, len(profile.Tokens))
	for _, token := range profile.Tokens {
		if containsStructuredToken(row.CoreSearchText, token) {
			continue
		}
		if narrativeText != "" && containsStructuredToken(narrativeText, token) {
			continue
		}
		if containsStructuredToken(row.SearchText, token) {
			out = append(out, token)
		}
	}
	return uniqueChatTokens(out)
}

func isProjectedSourceFieldRelevant(field structuredChatSourceField, profile chatQuestionProfile, unresolvedTokens []string) bool {
	if strings.TrimSpace(field.Raw) == "" {
		return false
	}

	if field.NormalizedName != "" && containsStructuredTokenSequence(profile.NormalizedQuestion, field.NormalizedName) {
		return true
	}

	for _, token := range strings.Fields(field.NormalizedName) {
		if containsStructuredToken(profile.NormalizedQuestion, token) {
			return true
		}
	}

	for _, token := range unresolvedTokens {
		if containsStructuredToken(field.NormalizedValue, token) {
			return true
		}
	}

	return false
}

func isProjectedNarrativeSourceField(field structuredChatSourceField) bool {
	switch field.NormalizedName {
	case "notes", "note", "additional information", "additional info", "death details", "death detail":
		return true
	default:
		return false
	}
}

func setBundleField(bundle map[string]any, key string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	bundle[key] = value
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
