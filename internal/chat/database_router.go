package chat

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	f "nordik-drive-api/internal/file"
)

type databaseChatOperation string

const (
	databaseOperationCount         databaseChatOperation = "count"
	databaseOperationExistence     databaseChatOperation = "existence"
	databaseOperationDistinct      databaseChatOperation = "distinct_values"
	databaseOperationDistinctCount databaseChatOperation = "distinct_count"
	databaseOperationGroupSummary  databaseChatOperation = "group_summary"
	databaseOperationGroupExtreme  databaseChatOperation = "group_extreme"
	databaseOperationRecordList    databaseChatOperation = "record_list"
	databaseOperationDatasetView   databaseChatOperation = "dataset_overview"
	databaseOperationFieldCatalog  databaseChatOperation = "field_catalog"
	maxFastChatAnswers                                   = 2048
)

type databaseChatPlan struct {
	Operation databaseChatOperation
	Field     string
	Extreme   string
	WantNames bool
}

type databaseDimensionValue struct {
	Normalized string `gorm:"column:normalized"`
	Display    string `gorm:"column:display"`
	Count      int    `gorm:"column:record_count"`
}

type databaseNamedRecord struct {
	SourceRowID int    `gorm:"column:source_row_id"`
	DisplayName string `gorm:"column:display_name"`
}

type databaseConfiguredValue struct {
	Display string `gorm:"column:display_value"`
	Count   int    `gorm:"column:record_count"`
}

func (cs *ChatService) tryDatabaseChat(input ChatQueryInput) (*ChatResult, bool) {
	start := time.Now()
	plan, ok := planDatabaseChatQuestion(input.Question)
	if !ok {
		return cs.tryConfiguredDatabaseDistinctChat(input, start)
	}
	if plan.Operation == databaseOperationFieldCatalog {
		route, ok := cs.executeDatabaseChatPlan(input, plan, deterministicQuestionContext{})
		if !ok {
			return nil, false
		}
		return databaseChatResult(input, route, start), true
	}

	ctx := buildDeterministicQuestionContext(nil, input.Question)
	if databaseHasUnsupportedFilters(ctx.Filters) {
		return nil, false
	}
	if !databasePlanSupportsContext(plan, ctx) {
		var resolved bool
		ctx, resolved = cs.buildDatabaseQuestionContext(input)
		if !resolved || !databasePlanSupportsContext(plan, ctx) {
			return nil, false
		}
	}

	route, ok := cs.executeDatabaseChatPlan(input, plan, ctx)
	if !ok {
		return nil, false
	}

	return databaseChatResult(input, route, start), true
}

func databaseChatResult(input ChatQueryInput, route deterministicChatRoute, start time.Time) *ChatResult {
	return &ChatResult{
		Answer:       route.Answer,
		MatchedRowID: route.MatchedRowID,
		Debug: &ChatDebugMetrics{
			Strategy:             "database_router",
			ExecutionMode:        "database",
			QueryType:            route.QueryType,
			RetrievalMode:        route.RetrievalMode,
			PromptProjectionMode: "database",
			Version:              input.Version,
			CommunityFilterCount: len(normalizeCommunities(input.Communities)),
			RowsSelected:         route.RowsSelected,
			PromptChars:          0,
			PromptBytes:          0,
			AudioIncluded:        false,
			PreparationMillis:    time.Since(start).Milliseconds(),
		},
	}
}

func (cs *ChatService) tryConfiguredDatabaseDistinctChat(input ChatQueryInput, start time.Time) (*ChatResult, bool) {
	normalizedQuestion := normalizeChatSearchValue(input.Question)
	if !looksLikeDeterministicDistinctValueListQuestion(normalizedQuestion) {
		return nil, false
	}

	field, ok := matchConfiguredDeterministicField(cs.loadConfiguredDeterministicFields(input.FileID), normalizedQuestion)
	if !ok {
		return nil, false
	}
	ctx := buildDeterministicQuestionContext(nil, input.Question)
	if databaseHasUnsupportedFilters(ctx.Filters) {
		return nil, false
	}
	if hasUnsupportedDistinctValueListTokens(ctx.Profile.Tokens, ctx.ConsumedTokens, configuredDeterministicFieldQuestionAliases(field)) {
		var resolved bool
		ctx, resolved = cs.buildDatabaseQuestionContext(input)
		if !resolved || hasUnsupportedDistinctValueListTokens(ctx.Profile.Tokens, ctx.ConsumedTokens, configuredDeterministicFieldQuestionAliases(field)) {
			return nil, false
		}
	}

	values, rowsSelected, err := cs.queryConfiguredDatabaseDistinctValues(input, field, ctx.Filters)
	if err != nil {
		return nil, false
	}
	displayLabel := firstNonEmptyString(field.Label, strings.ReplaceAll(field.Key, "_", " "), "configured field")
	answer := fmt.Sprintf("I couldn't find any values for %s in the matching data.", displayLabel)
	if len(values) == 1 {
		answer = fmt.Sprintf("The value for %s in the data is %s.", displayLabel, values[0])
	} else if len(values) > 1 {
		answer = fmt.Sprintf("I found %d distinct values for %s: %s.", len(values), displayLabel, joinNaturalStrings(values))
	}

	route := deterministicChatRoute{
		QueryType:     "distinct_values",
		RetrievalMode: "database_config_distinct_values",
		Answer:        answer,
		RowsSelected:  rowsSelected,
	}
	return databaseChatResult(input, route, start), true
}

func (cs *ChatService) getFastChatAnswer(input ChatQueryInput) (*ChatResult, bool) {
	if cs == nil {
		return nil, false
	}
	cs.sharedState().fastAnswerCacheMu.RLock()
	result := cs.sharedState().fastAnswerCache[fastChatAnswerCacheKey(input)]
	cloned := cloneChatResult(result)
	cs.sharedState().fastAnswerCacheMu.RUnlock()
	if cloned == nil {
		return nil, false
	}
	if cloned.Debug != nil {
		cloned.Debug.Strategy = "fast_answer_cache"
		cloned.Debug.ExecutionMode = "cache"
		cloned.Debug.PreparationMillis = 0
		cloned.Debug.GenerationMillis = 0
		cloned.Debug.TotalMillis = 0
		cloned.Debug.PrimaryModel = ""
		cloned.Debug.UsedModel = ""
	}
	return cloned, true
}

func (cs *ChatService) storeFastChatAnswer(input ChatQueryInput, result *ChatResult) {
	if cs == nil || result == nil {
		return
	}
	// A verified direct answer returned during a model timeout remains available
	// to the current callers, but is not cached so the next request can retry the
	// required LLM rendering pass.
	if result.Debug != nil && strings.HasSuffix(result.Debug.ExecutionMode, "_fallback") {
		return
	}
	key := fastChatAnswerCacheKey(input)
	cs.sharedState().fastAnswerCacheMu.Lock()
	defer cs.sharedState().fastAnswerCacheMu.Unlock()
	if cs.sharedState().fastAnswerCache == nil {
		cs.sharedState().fastAnswerCache = make(map[string]*ChatResult, maxFastChatAnswers)
	}
	if _, exists := cs.sharedState().fastAnswerCache[key]; !exists {
		if len(cs.sharedState().fastAnswerCacheOrder) >= maxFastChatAnswers {
			oldest := cs.sharedState().fastAnswerCacheOrder[0]
			delete(cs.sharedState().fastAnswerCache, oldest)
			cs.sharedState().fastAnswerCacheOrder = cs.sharedState().fastAnswerCacheOrder[1:]
		}
		cs.sharedState().fastAnswerCacheOrder = append(cs.sharedState().fastAnswerCacheOrder, key)
	}
	cs.sharedState().fastAnswerCache[key] = cloneChatResult(result)
}

func fastChatAnswerCacheKey(input ChatQueryInput) string {
	communities := normalizeCommunities(input.Communities)
	sort.Strings(communities)
	return fmt.Sprintf(
		"%d:%d:%s:%s:%s",
		input.FileID,
		input.Version,
		strings.Join(communities, "\x1f"),
		normalizeChatSearchValue(input.FileDescription),
		normalizeChatSearchValue(input.Question),
	)
}

func cloneChatResult(result *ChatResult) *ChatResult {
	if result == nil {
		return nil
	}
	cloned := *result
	if result.MatchedRowID != nil {
		matchedRowID := *result.MatchedRowID
		cloned.MatchedRowID = &matchedRowID
	}
	if result.Debug != nil {
		debug := *result.Debug
		cloned.Debug = &debug
	}
	return &cloned
}

// databaseDistinctValuesResult is the memoized result of a configured
// distinct-values aggregation.
type databaseDistinctValuesResult struct {
	Values       []string
	RowsSelected int
}

// databaseQueryCacheKey identifies a database aggregation by the pinned inputs
// that determine its result: file, version, community scope and row filters. It
// deliberately excludes the question wording, so different phrasings that map
// to the same aggregation share one cached result.
func databaseQueryCacheKey(namespace string, input ChatQueryInput, filters deterministicRowFilter) string {
	return fmt.Sprintf("%s\x1f%d\x1f%d\x1f%s\x1f%s",
		namespace,
		input.FileID,
		input.Version,
		strings.Join(normalizeCommunities(input.Communities), "\x1f"),
		deterministicRowFilterCacheKey(filters),
	)
}

func deterministicRowFilterCacheKey(filters deterministicRowFilter) string {
	return strings.Join([]string{
		filters.CommunityNormalized,
		filters.SchoolNormalized,
		filterStringPointerKey(filters.DeceasedStatus),
		filterBoolPointerKey(filters.RequireDeathRecord),
		filterBoolPointerKey(filters.RequireNotes),
		filterBoolPointerKey(filters.RequireAddInfo),
		filterBoolPointerKey(filters.RequireDeathDetails),
		filterBoolPointerKey(filters.RequirePhotos),
	}, "\x1f")
}

func filterStringPointerKey(value *string) string {
	if value == nil {
		return "\x00"
	}
	return "\x01" + *value
}

func filterBoolPointerKey(value *bool) string {
	if value == nil {
		return "\x00"
	}
	if *value {
		return "\x01true"
	}
	return "\x01false"
}

func planDatabaseChatQuestion(question string) (databaseChatPlan, bool) {
	normalizedQuestion := normalizeChatSearchValue(question)
	if normalizedQuestion == "" {
		return databaseChatPlan{}, false
	}
	if looksLikeDatabaseFieldCatalogQuestion(normalizedQuestion) {
		return databaseChatPlan{Operation: databaseOperationFieldCatalog}, true
	}
	if looksLikeDatabaseOverviewQuestion(normalizedQuestion) {
		return databaseChatPlan{Operation: databaseOperationDatasetView}, true
	}

	if extreme := deterministicGroupExtreme(normalizedQuestion); extreme != "" {
		if field, _, ok := detectDeterministicGroupField(normalizedQuestion); ok {
			return databaseChatPlan{
				Operation: databaseOperationGroupExtreme,
				Field:     field,
				Extreme:   extreme,
				WantNames: wantsDeterministicGroupMemberList(normalizedQuestion),
			}, true
		}
	}

	if looksLikeDeterministicGroupSummaryQuestion(normalizedQuestion) {
		if field, _, ok := detectDeterministicGroupField(normalizedQuestion); ok {
			return databaseChatPlan{Operation: databaseOperationGroupSummary, Field: field}, true
		}
	}

	if looksLikeDeterministicDistinctValueListQuestion(normalizedQuestion) {
		if field, _, ok := detectDeterministicGroupField(normalizedQuestion); ok {
			return databaseChatPlan{Operation: databaseOperationDistinct, Field: field}, true
		}
	}

	if looksLikeDeterministicCountQuestion(normalizedQuestion) && !mentionsDatabaseRecordNoun(normalizedQuestion) {
		if field, _, ok := detectDeterministicGroupField(normalizedQuestion); ok {
			return databaseChatPlan{Operation: databaseOperationDistinctCount, Field: field}, true
		}
	}

	if looksLikeDatabaseRecordListQuestion(normalizedQuestion) {
		return databaseChatPlan{Operation: databaseOperationRecordList}, true
	}
	if looksLikeDeterministicExistenceQuestion(normalizedQuestion) {
		return databaseChatPlan{Operation: databaseOperationExistence}, true
	}
	if looksLikeDeterministicCountQuestion(normalizedQuestion) {
		return databaseChatPlan{Operation: databaseOperationCount}, true
	}

	return databaseChatPlan{}, false
}

func (cs *ChatService) buildDatabaseQuestionContext(input ChatQueryInput) (deterministicQuestionContext, bool) {
	communities, err := cs.loadDatabaseDimensionValues(input, "community")
	if err != nil {
		return deterministicQuestionContext{}, false
	}
	schools, err := cs.loadDatabaseDimensionValues(input, "school")
	if err != nil {
		return deterministicQuestionContext{}, false
	}

	rows := make([]cachedStructuredChatRow, 0, len(communities)+len(schools))
	for _, value := range communities {
		rows = append(rows, cachedStructuredChatRow{
			CanonicalCommunity: value.Normalized,
			DefaultBundle: structuredChatDefaultBundle{
				Community: value.Display,
			},
		})
	}
	for _, value := range schools {
		rows = append(rows, cachedStructuredChatRow{
			CanonicalSchool: value.Normalized,
			DefaultBundle: structuredChatDefaultBundle{
				School: value.Display,
			},
		})
	}
	return buildDeterministicQuestionContext(rows, input.Question), true
}

func databasePlanSupportsContext(plan databaseChatPlan, ctx deterministicQuestionContext) bool {
	if databaseHasUnsupportedFilters(ctx.Filters) {
		return false
	}

	switch plan.Operation {
	case databaseOperationCount, databaseOperationExistence:
		return !hasUnsupportedDeterministicTokens(ctx.Profile.Tokens, ctx.ConsumedTokens)
	case databaseOperationDistinct, databaseOperationDistinctCount:
		return !hasUnsupportedDistinctValueListTokens(ctx.Profile.Tokens, ctx.ConsumedTokens, deterministicGroupQuestionAliases(plan.Field))
	case databaseOperationGroupSummary:
		field, consumed, ok := detectDeterministicGroupField(ctx.NormalizedQuestion)
		if !ok || field != plan.Field {
			return false
		}
		consumeTokens(ctx.ConsumedTokens, consumed...)
		return !hasUnsupportedDeterministicTokens(ctx.Profile.Tokens, ctx.ConsumedTokens)
	case databaseOperationGroupExtreme:
		field, consumed, ok := detectDeterministicGroupField(ctx.NormalizedQuestion)
		if !ok || field != plan.Field {
			return false
		}
		consumeTokens(ctx.ConsumedTokens, consumed...)
		consumeNormalizedPhrase(ctx.ConsumedTokens, plan.Extreme)
		if plan.WantNames {
			consumeTokens(ctx.ConsumedTokens, "name", "names", "who", "they")
		}
		return !hasUnsupportedDeterministicTokens(ctx.Profile.Tokens, ctx.ConsumedTokens)
	case databaseOperationRecordList:
		return !hasUnsupportedDatabaseRecordListTokens(ctx.Profile.Tokens, ctx.ConsumedTokens)
	case databaseOperationDatasetView:
		return !hasUnsupportedDatabaseOverviewTokens(ctx.Profile.Tokens, ctx.ConsumedTokens)
	case databaseOperationFieldCatalog:
		return true
	default:
		return false
	}
}

func databaseHasUnsupportedFilters(filters deterministicRowFilter) bool {
	return filters.DeceasedStatus != nil ||
		filters.RequireDeathRecord != nil ||
		filters.RequireNotes != nil ||
		filters.RequireAddInfo != nil ||
		filters.RequireDeathDetails != nil ||
		filters.RequirePhotos != nil
}

func (cs *ChatService) executeDatabaseChatPlan(input ChatQueryInput, plan databaseChatPlan, ctx deterministicQuestionContext) (deterministicChatRoute, bool) {
	switch plan.Operation {
	case databaseOperationFieldCatalog:
		fields := cs.loadConfiguredDeterministicFields(input.FileID)
		labels := make([]string, 0, len(fields))
		for _, field := range fields {
			label := strings.TrimSpace(firstNonEmptyString(field.Label, strings.ReplaceAll(field.Key, "_", " ")))
			if label != "" {
				labels = append(labels, label)
			}
		}
		labels = uniqueChatTokens(labels)
		sort.Slice(labels, func(i, j int) bool {
			return strings.ToLower(labels[i]) < strings.ToLower(labels[j])
		})
		if len(labels) == 0 {
			return deterministicChatRoute{}, false
		}
		answer := fmt.Sprintf("The %d configured fields are %s.", len(labels), joinNaturalStrings(labels))
		return deterministicChatRoute{QueryType: "field_catalog", RetrievalMode: "database_field_catalog", Answer: answer, RowsSelected: len(labels)}, true

	case databaseOperationDatasetView:
		return cs.buildDatabaseDatasetOverview(input, ctx.Filters)

	case databaseOperationCount, databaseOperationExistence:
		count, err := cs.countDatabaseRows(input, ctx.Filters)
		if err != nil {
			return deterministicChatRoute{}, false
		}
		if plan.Operation == databaseOperationExistence {
			answer := "No, I didn't find any matching records."
			if count > 0 {
				answer = fmt.Sprintf("Yes, I found %d matching record%s.", count, pluralSuffix(count))
			}
			return deterministicChatRoute{QueryType: "existence", RetrievalMode: "database_existence", Answer: answer, RowsSelected: count}, true
		}
		return deterministicChatRoute{
			QueryType:     "count",
			RetrievalMode: "database_count",
			Answer:        formatDeterministicCountAnswer(count, ctx.Filters, len(input.Communities) > 0),
			RowsSelected:  count,
		}, true

	case databaseOperationDistinct, databaseOperationDistinctCount, databaseOperationGroupSummary, databaseOperationGroupExtreme:
		var grouped []databaseDimensionValue
		var err error
		if databaseRowFiltersEmpty(ctx.Filters) && len(normalizeCommunities(input.Communities)) == 0 {
			grouped, err = cs.loadDatabaseDimensionValues(input, plan.Field)
		} else {
			grouped, err = cs.queryDatabaseDimensionCounts(input, plan.Field, ctx.Filters)
		}
		if err != nil {
			return deterministicChatRoute{}, false
		}
		rowsSelected := databaseGroupedRowCount(grouped)
		if plan.Operation == databaseOperationDistinctCount {
			dimension := deterministicGroupDimensionPlural(plan.Field)
			answer := fmt.Sprintf("I found %d distinct %s in the matching data.", len(grouped), dimension)
			return deterministicChatRoute{QueryType: "distinct_count", RetrievalMode: "database_distinct_count", Answer: answer, RowsSelected: rowsSelected}, true
		}
		if plan.Operation == databaseOperationDistinct {
			sort.Slice(grouped, func(i, j int) bool {
				return strings.ToLower(grouped[i].Display) < strings.ToLower(grouped[j].Display)
			})
			displays := databaseGroupedDisplays(grouped)
			dimension := deterministicGroupDimension(plan.Field)
			answer := fmt.Sprintf("I couldn't find any %s values in the matching data.", dimension)
			if len(displays) == 1 {
				answer = fmt.Sprintf("The %s in the data is %s.", dimension, displays[0])
			} else if len(displays) > 1 {
				answer = fmt.Sprintf("The %d %s in the data are %s.", len(displays), deterministicGroupDimensionPlural(plan.Field), joinNaturalStrings(displays))
			}
			return deterministicChatRoute{QueryType: "distinct_values", RetrievalMode: "database_distinct_values", Answer: answer, RowsSelected: rowsSelected}, true
		}
		if len(grouped) == 0 {
			return deterministicChatRoute{}, false
		}
		if plan.Operation == databaseOperationGroupSummary {
			sort.Slice(grouped, func(i, j int) bool {
				if grouped[i].Count != grouped[j].Count {
					return grouped[i].Count > grouped[j].Count
				}
				return strings.ToLower(grouped[i].Display) < strings.ToLower(grouped[j].Display)
			})
			parts := make([]string, 0, len(grouped))
			for _, item := range grouped {
				parts = append(parts, fmt.Sprintf("%s (%d)", item.Display, item.Count))
			}
			answer := fmt.Sprintf("By %s, the matching records break down as %s.", deterministicGroupDimension(plan.Field), joinNaturalStrings(parts))
			return deterministicChatRoute{QueryType: "group_summary", RetrievalMode: "database_group_summary", Answer: answer, RowsSelected: rowsSelected}, true
		}
		return cs.buildDatabaseGroupExtremeRoute(input, plan, ctx.Filters, grouped, rowsSelected)

	case databaseOperationRecordList:
		records, err := cs.queryDatabaseNamedRecords(input, ctx.Filters)
		if err != nil {
			return deterministicChatRoute{}, false
		}
		names := databaseRecordDisplayNames(records)
		answer := "I didn't find any matching records."
		if len(names) > 0 {
			answer = fmt.Sprintf("I found %d matching record%s: %s.", len(names), pluralSuffix(len(names)), joinNaturalStrings(names))
		}
		return deterministicChatRoute{QueryType: "record_list", RetrievalMode: "database_record_list", Answer: answer, RowsSelected: len(records)}, true
	}

	return deterministicChatRoute{}, false
}

func (cs *ChatService) buildDatabaseDatasetOverview(input ChatQueryInput, filters deterministicRowFilter) (deterministicChatRoute, bool) {
	count, err := cs.countDatabaseRows(input, filters)
	if err != nil {
		return deterministicChatRoute{}, false
	}

	queryDimension := func(field string) ([]databaseDimensionValue, error) {
		if databaseRowFiltersEmpty(filters) && len(normalizeCommunities(input.Communities)) == 0 {
			return cs.loadDatabaseDimensionValues(input, field)
		}
		return cs.queryDatabaseDimensionCounts(input, field, filters)
	}
	communities, err := queryDimension("community")
	if err != nil {
		return deterministicChatRoute{}, false
	}
	schools, err := queryDimension("school")
	if err != nil {
		return deterministicChatRoute{}, false
	}

	communityLabel := "communities"
	if len(communities) == 1 {
		communityLabel = "community"
	}
	answer := fmt.Sprintf("The matching data contains %d record%s across %d distinct %s and %d distinct school%s.",
		count,
		pluralSuffix(count),
		len(communities),
		communityLabel,
		len(schools),
		pluralSuffix(len(schools)),
	)
	if summary := formatDatabaseTopGroups("The largest community groups", communities, 5); summary != "" {
		answer += " " + summary
	}
	if summary := formatDatabaseTopGroups("The largest school groups", schools, 5); summary != "" {
		answer += " " + summary
	}

	return deterministicChatRoute{QueryType: "dataset_overview", RetrievalMode: "database_dataset_overview", Answer: answer, RowsSelected: count}, true
}

func formatDatabaseTopGroups(prefix string, values []databaseDimensionValue, limit int) string {
	if len(values) == 0 || limit <= 0 {
		return ""
	}
	values = append([]databaseDimensionValue(nil), values...)
	sort.Slice(values, func(i, j int) bool {
		if values[i].Count != values[j].Count {
			return values[i].Count > values[j].Count
		}
		return strings.ToLower(values[i].Display) < strings.ToLower(values[j].Display)
	})
	if len(values) > limit {
		values = values[:limit]
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("%s (%d)", value.Display, value.Count))
	}
	return fmt.Sprintf("%s are %s.", prefix, joinNaturalStrings(parts))
}

func (cs *ChatService) buildDatabaseGroupExtremeRoute(input ChatQueryInput, plan databaseChatPlan, filters deterministicRowFilter, grouped []databaseDimensionValue, rowsSelected int) (deterministicChatRoute, bool) {
	sort.Slice(grouped, func(i, j int) bool {
		if grouped[i].Count != grouped[j].Count {
			if plan.Extreme == "lowest" {
				return grouped[i].Count < grouped[j].Count
			}
			return grouped[i].Count > grouped[j].Count
		}
		return strings.ToLower(grouped[i].Display) < strings.ToLower(grouped[j].Display)
	})

	targetCount := grouped[0].Count
	tied := make([]databaseDimensionValue, 0, len(grouped))
	for _, item := range grouped {
		if item.Count != targetCount {
			break
		}
		tied = append(tied, item)
	}

	displays := databaseGroupedDisplays(tied)
	answer := ""
	if len(displays) == 1 {
		answer = fmt.Sprintf("%s has the %s number of matching records, with %d matching record%s.", displays[0], plan.Extreme, targetCount, pluralSuffix(targetCount))
	} else {
		answer = fmt.Sprintf("%s tie for the %s number of matching records, with %d matching record%s each.", joinNaturalStrings(displays), plan.Extreme, targetCount, pluralSuffix(targetCount))
	}

	if plan.WantNames {
		parts := make([]string, 0, len(tied))
		for _, item := range tied {
			groupFilters := filters
			if plan.Field == "community" {
				groupFilters.CommunityNormalized = item.Normalized
				groupFilters.CommunityDisplay = item.Display
			} else {
				groupFilters.SchoolNormalized = item.Normalized
				groupFilters.SchoolDisplay = item.Display
			}
			records, err := cs.queryDatabaseNamedRecords(input, groupFilters)
			if err != nil {
				return deterministicChatRoute{}, false
			}
			names := databaseRecordDisplayNames(records)
			if len(names) > 0 {
				parts = append(parts, fmt.Sprintf("For %s, the matching names are %s", item.Display, joinNaturalStrings(names)))
			}
		}
		if len(parts) > 0 {
			answer += " " + strings.Join(parts, ". ") + "."
		}
	}

	return deterministicChatRoute{QueryType: "group_extreme", RetrievalMode: "database_group_extreme", Answer: answer, RowsSelected: rowsSelected}, true
}

func (cs *ChatService) databaseBaseQuery(input ChatQueryInput) *gorm.DB {
	return cs.DB.Table("file_data_normalized").
		Where("file_id = ? AND version = ? AND status = ? AND normalization_version = ?", input.FileID, input.Version, "ready", f.CurrentNormalizationVersion())
}

func (cs *ChatService) loadDatabaseDimensionValues(input ChatQueryInput, field string) ([]databaseDimensionValue, error) {
	return cs.queryDatabaseDimensionCounts(input, field, deterministicRowFilter{})
}

func (cs *ChatService) queryDatabaseDimensionCounts(input ChatQueryInput, field string, filters deterministicRowFilter) ([]databaseDimensionValue, error) {
	cacheKey := databaseQueryCacheKey("dimension", input, filters) + "\x1f" + field
	if cached, ok := cs.sharedState().databaseQueryCache.load(cacheKey); ok {
		if values, ok := cached.([]databaseDimensionValue); ok {
			return append([]databaseDimensionValue(nil), values...), nil
		}
	}

	column, displayExpression, ok := databaseDimensionSQL(field)
	if !ok {
		return nil, fmt.Errorf("unsupported database dimension %q", field)
	}

	var values []databaseDimensionValue
	query := applyDatabaseChatFilters(cs.databaseBaseQuery(input), input.Communities, filters).
		Select(fmt.Sprintf("%s AS normalized, %s AS display, COUNT(*) AS record_count", column, displayExpression)).
		Where(column + " <> ''").
		Group(column)
	if err := query.Scan(&values).Error; err != nil {
		return nil, err
	}
	for index := range values {
		values[index].Normalized = normalizeChatSearchValue(values[index].Normalized)
		values[index].Display = structuredFieldDisplay(values[index].Display, values[index].Normalized)
	}
	cs.sharedState().databaseQueryCache.store(cacheKey, append([]databaseDimensionValue(nil), values...))
	return values, nil
}

func databaseDimensionSQL(field string) (string, string, bool) {
	switch field {
	case "community":
		return "canonical_community", "COALESCE(MIN(NULLIF(row_data_normalized #>> '{chat,default_bundle,community}', '')), canonical_community)", true
	case "school":
		return "canonical_school", "COALESCE(MIN(NULLIF(row_data_normalized #>> '{chat,default_bundle,school}', '')), canonical_school)", true
	default:
		return "", "", false
	}
}

func applyDatabaseChatFilters(query *gorm.DB, communities []string, filters deterministicRowFilter) *gorm.DB {
	if normalizedCommunities := normalizeCommunities(communities); len(normalizedCommunities) > 0 {
		query = query.Where("canonical_community IN ?", normalizedCommunities)
	}
	if filters.CommunityNormalized != "" {
		query = query.Where("canonical_community = ?", filters.CommunityNormalized)
	}
	if filters.SchoolNormalized != "" {
		query = query.Where("canonical_school = ?", filters.SchoolNormalized)
	}
	return query
}

func databaseRowFiltersEmpty(filters deterministicRowFilter) bool {
	return filters.CommunityNormalized == "" &&
		filters.SchoolNormalized == "" &&
		filters.DeceasedStatus == nil &&
		filters.RequireDeathRecord == nil &&
		filters.RequireNotes == nil &&
		filters.RequireAddInfo == nil &&
		filters.RequireDeathDetails == nil &&
		filters.RequirePhotos == nil
}

func (cs *ChatService) countDatabaseRows(input ChatQueryInput, filters deterministicRowFilter) (int, error) {
	cacheKey := databaseQueryCacheKey("count", input, filters)
	if cached, ok := cs.sharedState().databaseQueryCache.load(cacheKey); ok {
		if count, ok := cached.(int); ok {
			return count, nil
		}
	}
	var count int64
	if err := applyDatabaseChatFilters(cs.databaseBaseQuery(input), input.Communities, filters).Count(&count).Error; err != nil {
		return 0, err
	}
	result := int(count)
	cs.sharedState().databaseQueryCache.store(cacheKey, result)
	return result, nil
}

func (cs *ChatService) queryDatabaseNamedRecords(input ChatQueryInput, filters deterministicRowFilter) ([]databaseNamedRecord, error) {
	cacheKey := databaseQueryCacheKey("records", input, filters)
	if cached, ok := cs.sharedState().databaseQueryCache.load(cacheKey); ok {
		if records, ok := cached.([]databaseNamedRecord); ok {
			return append([]databaseNamedRecord(nil), records...), nil
		}
	}
	const displayNameExpression = "COALESCE(NULLIF(row_data_normalized #>> '{chat,default_bundle,name}', ''), NULLIF(row_data_normalized #>> '{canonical,display_name}', ''), canonical_name)"
	var records []databaseNamedRecord
	query := applyDatabaseChatFilters(cs.databaseBaseQuery(input), input.Communities, filters).
		Select("source_row_id, " + displayNameExpression + " AS display_name").
		Order("canonical_name ASC").
		Order("source_row_id ASC")
	if err := query.Scan(&records).Error; err != nil {
		return nil, err
	}
	cs.sharedState().databaseQueryCache.store(cacheKey, append([]databaseNamedRecord(nil), records...))
	return records, nil
}

func (cs *ChatService) queryConfiguredDatabaseDistinctValues(input ChatQueryInput, field configuredDeterministicField, filters deterministicRowFilter) ([]string, int, error) {
	if len(field.SourceNames) == 0 {
		return nil, 0, fmt.Errorf("configured field has no source names")
	}

	cacheKey := databaseQueryCacheKey("distinct", input, filters) + "\x1f" + strings.Join(field.SourceNames, "\x1e")
	if cached, ok := cs.sharedState().databaseQueryCache.load(cacheKey); ok {
		if result, ok := cached.(databaseDistinctValuesResult); ok {
			return append([]string(nil), result.Values...), result.RowsSelected, nil
		}
	}

	valueParts := make([]string, 0, len(field.SourceNames))
	valueArgs := make([]any, 0, len(field.SourceNames))
	for _, sourceName := range field.SourceNames {
		valueParts = append(valueParts, "NULLIF(jsonb_extract_path_text(row_data_normalized, 'fields', ?, 'raw'), '')")
		valueArgs = append(valueArgs, sourceName)
	}
	valueExpression := "COALESCE(" + strings.Join(valueParts, ", ") + ", '')"
	subQuery := applyDatabaseChatFilters(cs.databaseBaseQuery(input), input.Communities, filters).
		Select(valueExpression+" AS display_value", valueArgs...)

	var rawValues []databaseConfiguredValue
	query := cs.DB.Table("(?) AS configured_values", subQuery).
		Select("display_value, COUNT(*) AS record_count").
		Where("display_value <> ''").
		Group("display_value")
	if err := query.Scan(&rawValues).Error; err != nil {
		return nil, 0, err
	}

	counts := make(map[string]databaseConfiguredValue, len(rawValues))
	for _, value := range rawValues {
		display := strings.TrimSpace(value.Display)
		normalized := normalizeChatSearchValue(display)
		if display == "" || normalized == "" {
			continue
		}
		item := counts[normalized]
		if item.Display == "" {
			item.Display = display
		}
		item.Count += value.Count
		counts[normalized] = item
	}

	values := make([]string, 0, len(counts))
	rowsSelected := 0
	for _, value := range counts {
		values = append(values, value.Display)
		rowsSelected += value.Count
	}
	sort.Slice(values, func(i, j int) bool {
		return strings.ToLower(values[i]) < strings.ToLower(values[j])
	})
	cs.sharedState().databaseQueryCache.store(cacheKey, databaseDistinctValuesResult{
		Values:       append([]string(nil), values...),
		RowsSelected: rowsSelected,
	})
	return values, rowsSelected, nil
}

func looksLikeDatabaseRecordListQuestion(normalizedQuestion string) bool {
	if strings.HasPrefix(normalizedQuestion, "who is from ") ||
		strings.HasPrefix(normalizedQuestion, "who are from ") ||
		strings.HasPrefix(normalizedQuestion, "who attended ") ||
		strings.HasPrefix(normalizedQuestion, "names from ") ||
		strings.HasPrefix(normalizedQuestion, "names in ") {
		return true
	}
	if !mentionsDatabaseRecordNoun(normalizedQuestion) {
		return false
	}
	return strings.HasPrefix(normalizedQuestion, "list ") ||
		strings.HasPrefix(normalizedQuestion, "list all ") ||
		strings.HasPrefix(normalizedQuestion, "show me ") ||
		strings.HasPrefix(normalizedQuestion, "show ") ||
		strings.HasPrefix(normalizedQuestion, "show all ") ||
		strings.HasPrefix(normalizedQuestion, "give me ") ||
		strings.HasPrefix(normalizedQuestion, "give me all ") ||
		strings.HasPrefix(normalizedQuestion, "what are the names") ||
		strings.HasPrefix(normalizedQuestion, "who are the") ||
		strings.HasPrefix(normalizedQuestion, "students from ") ||
		strings.HasPrefix(normalizedQuestion, "students in ") ||
		strings.HasPrefix(normalizedQuestion, "students at ")
}

func looksLikeDatabaseOverviewQuestion(normalizedQuestion string) bool {
	return questionMentionsAny(
		normalizedQuestion,
		"summarize data", "summarize dataset",
		"summarize the data", "summarize this data", "summarize the dataset", "summarize this dataset",
		"summary of the data", "summary of this data", "summary of the dataset", "summary of this dataset",
		"overview of the data", "overview of this data", "overview of the dataset", "overview of this dataset",
		"what does the data contain", "what does this data contain", "describe the dataset",
	)
}

func looksLikeDatabaseFieldCatalogQuestion(normalizedQuestion string) bool {
	return questionMentionsAny(
		normalizedQuestion,
		"what fields are available", "which fields are available", "available fields", "list fields", "list all fields",
		"what columns are available", "which columns are available", "available columns", "list columns", "list all columns",
		"what are the field names", "what are the column names",
	)
}

func mentionsDatabaseRecordNoun(normalizedQuestion string) bool {
	return questionMentionsAny(
		normalizedQuestion,
		"record", "records", "entry", "entries", "student", "students", "child", "children",
		"person", "people", "name", "names", "individual", "individuals",
	)
}

func hasUnsupportedDatabaseRecordListTokens(tokens []string, consumed map[string]struct{}) bool {
	allowed := map[string]struct{}{
		"all": {}, "attend": {}, "attended": {}, "child": {}, "children": {}, "entries": {}, "entry": {}, "every": {}, "give": {},
		"individual": {}, "individuals": {}, "list": {}, "lists": {}, "name": {}, "names": {}, "people": {},
		"person": {}, "record": {}, "records": {}, "show": {}, "student": {}, "students": {}, "who": {},
	}
	for _, token := range tokens {
		if _, ok := consumed[token]; ok {
			continue
		}
		if _, ok := allowed[token]; ok {
			continue
		}
		return true
	}
	return false
}

func hasUnsupportedDatabaseOverviewTokens(tokens []string, consumed map[string]struct{}) bool {
	allowed := map[string]struct{}{
		"contain": {}, "contains": {}, "data": {}, "dataset": {}, "describe": {}, "file": {},
		"give": {}, "overview": {}, "show": {}, "summarize": {}, "summary": {}, "tell": {},
	}
	for _, token := range tokens {
		if _, ok := consumed[token]; ok {
			continue
		}
		if _, ok := allowed[token]; ok {
			continue
		}
		return true
	}
	return false
}

func databaseGroupedRowCount(values []databaseDimensionValue) int {
	total := 0
	for _, value := range values {
		total += value.Count
	}
	return total
}

func databaseGroupedDisplays(values []databaseDimensionValue) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value.Display) != "" {
			out = append(out, value.Display)
		}
	}
	return out
}

func databaseRecordDisplayNames(records []databaseNamedRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		name := strings.TrimSpace(record.DisplayName)
		if name == "" {
			name = fmt.Sprintf("record %d", record.SourceRowID)
		}
		out = append(out, name)
	}
	return out
}
