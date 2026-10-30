package logs

import (
	"math"
	"nordik-drive-api/internal/util"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	"gorm.io/gorm"
)

type LogService struct {
	DB *gorm.DB
}

func (ls *LogService) Log(log SystemLog, metadata interface{}) error {
	metaStr := safeMetadata(metadata)

	newLog := SystemLog{
		Level:       log.Level,
		Service:     log.Service,
		UserID:      log.UserID,
		Action:      log.Action,
		Message:     log.Message,
		Metadata:    metaStr,
		CreatedAt:   time.Now(),
		Communities: log.Communities,
		Filename:    log.Filename,
	}

	return ls.DB.Create(&newLog).Error

}

func (ls *LogService) GetLogs(input LogFilterInput) ([]LogRow, LogAggregates, int64, int, error) {
	// Defaults
	if input.Page <= 0 {
		input.Page = 1
	}
	if input.PageSize <= 0 || input.PageSize > 100 {
		input.PageSize = 20
	}

	// Base query (joins users for name + supports search)
	base := ls.DB.
		Table("logs").
		Select("logs.*, a.firstname as firstname, a.lastname as lastname").
		Joins("LEFT JOIN users a ON logs.user_id = a.id")

	// Default: last 30 days if no dates
	if input.StartDate == nil && input.EndDate == nil {
		base = base.Where("logs.created_at >= ?", time.Now().AddDate(0, 0, -30))
	}

	// Filters
	if input.UserID != nil {
		base = base.Where("logs.user_id = ?", *input.UserID)
	}
	if input.Level != nil && strings.TrimSpace(*input.Level) != "" {
		base = base.Where("logs.level = ?", strings.TrimSpace(*input.Level))
	}
	if input.Service != nil && strings.TrimSpace(*input.Service) != "" {
		base = base.Where("logs.service = ?", strings.TrimSpace(*input.Service))
	}
	if input.Action != nil && strings.TrimSpace(*input.Action) != "" {
		base = base.Where("logs.action = ?", strings.TrimSpace(*input.Action))
	}

	// ✅ Filename filter (ILIKE, optional)
	if input.Filename != nil && strings.TrimSpace(*input.Filename) != "" {
		base = base.Where("COALESCE(logs.filename,'') ILIKE ?", "%"+strings.TrimSpace(*input.Filename)+"%")
	}

	// ✅ Communities filter: overlap (ANY match) - optional
	if len(input.Communities) > 0 {
		base = base.Where("logs.communities && ?", pq.Array(input.Communities))
	}

	// ✅ Date range (robust inclusive end-day)
	start, hasStart, endExclusive, hasEnd, err := util.ParseDateRange(input.StartDate, input.EndDate)
	if err != nil {
		return nil, LogAggregates{}, 0, 0, err
	}
	if hasStart {
		base = base.Where("logs.created_at >= ?", start)
	}
	if hasEnd {
		base = base.Where("logs.created_at < ?", endExclusive)
	}

	// ✅ Search across multiple columns (including filename + communities + person name)
	if input.Search != nil && strings.TrimSpace(*input.Search) != "" {
		like := "%" + strings.TrimSpace(*input.Search) + "%"
		base = base.Where(
			`CAST(logs.id AS TEXT) ILIKE ?
			 OR logs.level ILIKE ?
			 OR logs.service ILIKE ?
			 OR logs.action ILIKE ?
			 OR logs.message ILIKE ?
			 OR COALESCE(logs.filename,'') ILIKE ?
			 OR COALESCE(array_to_string(logs.communities, ','),'') ILIKE ?
			 OR COALESCE(a.firstname,'') ILIKE ?
			 OR COALESCE(a.lastname,'') ILIKE ?`,
			like, like, like, like, like, like, like, like, like,
		)
	}

	// ✅ Total count (no paging)
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, LogAggregates{}, 0, 0, err
	}

	totalPages := int(math.Ceil(float64(total) / float64(input.PageSize)))
	if totalPages == 0 {
		totalPages = 1
	}

	// ✅ Paged logs
	var rows []LogRow
	if err := base.
		Session(&gorm.Session{}).
		Order("logs.created_at DESC").
		Limit(input.PageSize).
		Offset((input.Page - 1) * input.PageSize).
		Scan(&rows).Error; err != nil {
		return nil, LogAggregates{}, 0, 0, err
	}

	// ✅ Aggregates from same filtered base
	aggs, err := ls.getAggregatesFromBase(base)
	if err != nil {
		return nil, LogAggregates{}, 0, 0, err
	}

	return rows, aggs, total, totalPages, nil
}

func (ls *LogService) getAggregatesFromBase(base *gorm.DB) (LogAggregates, error) {
	aggs := LogAggregates{}
	limit := 12

	// Use derived table so filters are identical
	sub := base.Session(&gorm.Session{}).
		Select("logs.user_id, logs.filename, logs.communities, a.firstname, a.lastname")

	derived := ls.DB.Table("(?) as x", sub)

	// -----------------------------
	// 1) By filename
	// -----------------------------
	{
		type r struct {
			Label string
			Count int64
		}
		var out []r

		if err := derived.Session(&gorm.Session{}).
			Select("COALESCE(NULLIF(TRIM(x.filename), ''), 'No filename') AS label, COUNT(*) AS count").
			Group("label").
			Order("count DESC").
			Limit(limit).
			Scan(&out).Error; err != nil {
			return LogAggregates{}, err
		}

		aggs.ByFilename = make([]AggItem, 0, len(out))
		for _, row := range out {
			aggs.ByFilename = append(aggs.ByFilename, AggItem{Label: row.Label, Count: row.Count})
		}
	}

	// -----------------------------
	// 2) By person (user)
	// -----------------------------
	{
		type r struct {
			UserID    *uint
			Firstname string
			Lastname  string
			Label     string
			Count     int64
		}
		var out []r

		if err := derived.Session(&gorm.Session{}).
			Select(`
				x.user_id,
				COALESCE(x.firstname,'') AS firstname,
				COALESCE(x.lastname,'') AS lastname,
				CASE
					WHEN (COALESCE(x.firstname,'') = '' AND COALESCE(x.lastname,'') = '')
					THEN 'Unknown'
					ELSE TRIM(COALESCE(x.firstname,'') || ' ' || COALESCE(x.lastname,''))
				END AS label,
				COUNT(*) AS count
			`).
			Group("x.user_id, firstname, lastname, label").
			Order("count DESC").
			Limit(limit).
			Scan(&out).Error; err != nil {
			return LogAggregates{}, err
		}

		aggs.ByPerson = make([]PersonAggItem, 0, len(out))
		for _, row := range out {
			aggs.ByPerson = append(aggs.ByPerson, PersonAggItem{
				UserID:    row.UserID,
				Firstname: row.Firstname,
				Lastname:  row.Lastname,
				Label:     row.Label,
				Count:     row.Count,
			})
		}
	}

	// -----------------------------
	// 3) By community
	//    - Properly unnest text[]
	//    - Also count rows with empty/null community as "No community"
	// -----------------------------
	{
		type r struct {
			Label string
			Count int64
		}

		// 3a) unnest communities
		var outA []r
		if err := derived.Session(&gorm.Session{}).
			// LATERAL unnest
			Select("c AS label, COUNT(*) AS count").
			Joins("JOIN LATERAL unnest(x.communities) AS c ON TRUE").
			Group("c").
			Order("count DESC").
			Limit(limit).
			Scan(&outA).Error; err != nil {
			return LogAggregates{}, err
		}

		// 3b) count empty/null array
		var outB []r
		if err := derived.Session(&gorm.Session{}).
			Select("'No community' AS label, COUNT(*) AS count").
			Where("x.communities IS NULL OR array_length(x.communities, 1) IS NULL OR array_length(x.communities, 1) = 0").
			Group("label").
			Scan(&outB).Error; err != nil {
			return LogAggregates{}, err
		}

		// merge (and re-sort)
		m := map[string]int64{}
		for _, row := range outA {
			m[row.Label] += row.Count
		}
		for _, row := range outB {
			m[row.Label] += row.Count
		}

		items := make([]AggItem, 0, len(m))
		for k, v := range m {
			items = append(items, AggItem{Label: k, Count: v})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Count > items[j].Count })
		if len(items) > limit {
			items = items[:limit]
		}
		aggs.ByCommunity = items
	}

	return aggs, nil
}
