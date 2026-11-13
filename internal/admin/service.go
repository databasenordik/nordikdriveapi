package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/requestscope"
)

const maxMediaZipFiles = 5000

type AdminService struct {
	DB        *gorm.DB
	Directory identityclient.Directory

	// OPTIONAL: for tests to stub paging/search without DB
	searchHook func(req AdminFileEditSearchRequest) (*AdminSearchResponse, error)
}

func (as *AdminService) SearchFileEditRequests(req AdminFileEditSearchRequest) (*AdminSearchResponse, error) {
	if as.searchHook != nil {
		return as.searchHook(req)
	}
	return as.searchChanges(req)
}

// ==========================
// CHANGES (requests only)
// ==========================

func (as *AdminService) searchChanges(req AdminFileEditSearchRequest) (*AdminSearchResponse, error) {
	// 1) base request query (NO join with details!)
	reqBase := as.DB.Table("file_edit_request r").
		Joins("LEFT JOIN file f ON f.id = r.file_id")
	if as.Directory == nil {
		reqBase = reqBase.Joins("LEFT JOIN users u_req ON u_req.id = r.user_id").Joins("LEFT JOIN users u_app ON u_app.id = r.reviewed_by")
	}

	// split clauses: request-level vs detail-level
	reqClauses, detailClauses := splitClauses(req.Clauses)

	// apply request filters
	var err error
	reqBase, err = applyRequestFilters(reqBase, reqClauses, "r")
	if err != nil {
		return nil, err
	}

	// subquery of request_ids that match request-level filters
	reqIDsSubq := reqBase.Session(&gorm.Session{}).
		Select("r.request_id")

	// 2) details query (always used to compute total_changes + per-request change_count)
	// ignore empty rows where both old/new are empty
	detailQ := as.DB.Table("file_edit_request_details d").
		Where("d.request_id IN (?)", reqIDsSubq).
		Where("(COALESCE(d.old_value,'') <> '' OR COALESCE(d.new_value,'') <> '')")

	// apply detail filters (field_key / old_value / new_value)
	detailQ, err = applyDetailFilters(detailQ, detailClauses, "d")
	if err != nil {
		return nil, err
	}

	// 3) total_changes = count of matching detail rows (non-empty)
	var totalChanges int64
	if err := detailQ.Session(&gorm.Session{}).Count(&totalChanges).Error; err != nil {
		return nil, err
	}

	hasDetailFilters := len(detailClauses) > 0

	var finalReqQ *gorm.DB
	if hasDetailFilters {
		matchedReqIDsSubq := detailQ.Session(&gorm.Session{}).Select("DISTINCT d.request_id")
		finalReqQ = reqBase.Where("r.request_id IN (?)", matchedReqIDsSubq)
	} else {
		finalReqQ = reqBase
	}

	// total request rows
	var totalRequests int64
	if err := finalReqQ.Session(&gorm.Session{}).Distinct("r.request_id").Count(&totalRequests).Error; err != nil {
		return nil, err
	}

	// 6) aggregations (ONLY ONE at a time)
	aggs := Aggregations{}

	if hasFileFilter(req.Clauses) {
		// by field (from matching details)
		var out []AggKV
		if err := detailQ.Session(&gorm.Session{}).
			Select("d.field_name AS key, COUNT(*) AS count").
			Group("d.field_name").
			Order("count DESC").
			Limit(50).
			Scan(&out).Error; err != nil {
			return nil, err
		}
		if len(out) > 0 {
			aggs.ByField = out
		} else {
			aggs.ByField = nil
		}
		aggs.ByFile = nil
	} else {
		// by file (requests)
		var out []AggKV
		if err := finalReqQ.Session(&gorm.Session{}).
			Select("COALESCE(f.filename,'(unknown)') AS key, COUNT(DISTINCT r.request_id) AS count").
			Group("f.filename").
			Order("count DESC").
			Limit(50).
			Scan(&out).Error; err != nil {
			return nil, err
		}
		if len(out) > 0 {
			aggs.ByFile = out
		} else {
			aggs.ByFile = nil
		}
		aggs.ByField = nil
	}

	// 7) page rows
	offset := (req.Page - 1) * req.PageSize

	// per-request change_count from the SAME filtered detailQ
	detailCountsSubq := detailQ.Session(&gorm.Session{}).
		Select("d.request_id, COUNT(*) AS change_count").
		Group("d.request_id")

	var rows []AdminChangeRow
	columns := `
			r.request_id, r.status, r.file_id, COALESCE(f.filename,'') AS file_name,
			COALESCE(r.firstname,'') AS firstname, COALESCE(r.lastname,'') AS lastname,
			COALESCE(r.community, '{}'::text[]) AS community,
			COALESCE(r.uploader_community, '{}'::text[]) AS uploader_community,
			'' AS requested_by, '' AS reviewed_by,
			r.user_id AS requested_by_id, r.reviewed_by AS reviewed_by_id,
			r.consent, COALESCE(dc.change_count,0) AS change_count, r.created_at`
	if as.Directory == nil {
		columns = strings.Replace(columns, "'' AS requested_by", "COALESCE(u_req.firstname || ' ' || u_req.lastname, 'User ' || r.user_id::text) AS requested_by", 1)
		columns = strings.Replace(columns, "'' AS reviewed_by", "COALESCE(u_app.firstname || ' ' || u_app.lastname, '') AS reviewed_by", 1)
	}
	if err := finalReqQ.Session(&gorm.Session{}).
		Joins("LEFT JOIN (?) dc ON dc.request_id = r.request_id", detailCountsSubq).
		Select(columns).
		Order("r.created_at DESC").
		Limit(req.PageSize).
		Offset(offset).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	if as.Directory != nil {
		ids := []int64{}
		for _, row := range rows {
			if row.RequestedByID != nil && *row.RequestedByID > 0 {
				ids = append(ids, int64(*row.RequestedByID))
			}
			if row.ReviewedByID != nil && *row.ReviewedByID > 0 {
				ids = append(ids, int64(*row.ReviewedByID))
			}
		}
		if len(ids) > 0 {
			profiles, err := as.Directory.Profiles(requestscope.Context(as.DB), ids, false)
			if err != nil {
				return nil, err
			}
			for i := range rows {
				row := &rows[i]
				if row.RequestedByID != nil {
					row.RequestedBy = fmt.Sprintf("User %d", *row.RequestedByID)
					if p, found := profiles[int64(*row.RequestedByID)]; found {
						row.RequestedBy = p.Firstname + " " + p.Lastname
					}
				}
				if row.ReviewedByID != nil {
					if p, found := profiles[int64(*row.ReviewedByID)]; found {
						row.ReviewedBy = p.Firstname + " " + p.Lastname
					}
				}
			}
		}
	}

	totalPages := int(math.Ceil(float64(totalRequests) / float64(req.PageSize)))
	if totalPages == 0 {
		totalPages = 1
	}

	return &AdminSearchResponse{
		Message:      "success",
		Page:         req.Page,
		PageSize:     req.PageSize,
		TotalPages:   totalPages,
		TotalRows:    totalRequests, // ✅ requests count
		TotalChanges: totalChanges,  // ✅ change rows count
		Aggregations: aggs,
		Data:         rows,
	}, nil
}

// ==========================
// DETAILS (one request)
// ==========================

func (as *AdminService) GetFileEditRequestDetails(requestID uint) ([]AdminChangeDetailRow, error) {
	var out []AdminChangeDetailRow
	if err := as.DB.Table("file_edit_request_details d").
		Select(`
			d.field_name AS field_key,
			COALESCE(d.old_value,'') AS old_value,
			COALESCE(d.new_value,'') AS new_value,
			COALESCE(d.status,'pending') AS status,
			COALESCE(d.reviewer_comment,'') AS reviewer_comment
		`).
		Where("d.request_id = ?", requestID).
		Where("(COALESCE(d.old_value,'') <> '' OR COALESCE(d.new_value,'') <> '')").
		Order("d.id ASC").
		Scan(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ==========================
// Filters
// ==========================

func splitClauses(clauses []Clause) (reqClauses []Clause, detailClauses []Clause) {
	for _, c := range clauses {
		switch c.Field {
		case "field_key", "old_value", "new_value":
			detailClauses = append(detailClauses, c)
		default:
			reqClauses = append(reqClauses, c)
		}
	}
	return
}

func hasFileFilter(clauses []Clause) bool {
	for _, c := range clauses {
		if c.Field != "file_id" {
			continue
		}
		if c.Op == OpEQ && c.Value != nil && strings.TrimSpace(*c.Value) != "" {
			return true
		}
		if c.Op == OpIN && len(c.Values) > 0 {
			return true
		}
	}
	return false
}

func applyRequestFilters(q *gorm.DB, clauses []Clause, rAlias string) (*gorm.DB, error) {
	for _, c := range clauses {
		switch c.Field {
		case "status":
			q = applyStringOp(q, rAlias+".status", c)
		case "file_id":
			q = applyIntOp(q, rAlias+".file_id", c)
		case "requested_by":
			q = applyIntOp(q, rAlias+".user_id", c)
		case "reviewed_by":
			q = applyIntOp(q, rAlias+".reviewed_by", c)
		case "consent":
			q = applyBoolOp(q, rAlias+".consent", c)

		case "firstname":
			q = applyStringOp(q, rAlias+".firstname", c)
		case "lastname":
			q = applyStringOp(q, rAlias+".lastname", c)

		case "community":
			q = applyTextArrayOp(q, rAlias+".community", c)

		case "uploader_community":
			q = applyTextArrayOp(q, rAlias+".uploader_community", c)

		case "created_at":
			var err error
			q, err = applyDateOp(q, rAlias+".created_at", c)
			if err != nil {
				return nil, err
			}
		}
	}
	return q, nil
}

func applyDetailFilters(q *gorm.DB, clauses []Clause, dAlias string) (*gorm.DB, error) {
	for _, c := range clauses {
		switch c.Field {
		case "field_key":
			q = applyStringOp(q, dAlias+".field_name", c)
		case "old_value":
			q = applyStringOp(q, dAlias+".old_value", c)
		case "new_value":
			q = applyStringOp(q, dAlias+".new_value", c)
		}
	}
	return q, nil
}

// ==========================
// Generic op helpers
// ==========================

func applyStringOp(q *gorm.DB, col string, c Clause) *gorm.DB {
	switch c.Op {
	case OpEQ:
		if c.Value != nil {
			return q.Where(col+" = ?", *c.Value)
		}
	case OpNEQ:
		if c.Value != nil {
			return q.Where(col+" <> ?", *c.Value)
		}
	case OpCONTAINS:
		if c.Value != nil {
			return q.Where(col+" ILIKE ?", "%"+*c.Value+"%")
		}
	case OpIN:
		if len(c.Values) > 0 {
			return q.Where(col+" IN ?", c.Values)
		}
	}
	return q
}

// Postgres text[] contains / overlaps helpers
// Robust Postgres text[] filter: works even if UI sends EQ with Values or IN with Value.
// Also does case-insensitive + trims spaces (so "xyz" matches " XYZ ").
func applyTextArrayOp(q *gorm.DB, col string, c Clause) *gorm.DB {
	switch c.Op {
	case OpEQ:
		if c.Value != nil {
			v := strings.TrimSpace(*c.Value)
			if v != "" {
				return q.Where(col+" @> ARRAY[?]::text[]", v)
			}
		}
	case OpIN:
		if len(c.Values) > 0 {
			vals := make([]string, 0, len(c.Values))
			for _, s := range c.Values {
				s = strings.TrimSpace(s)
				if s != "" {
					vals = append(vals, s)
				}
			}
			if len(vals) > 0 {
				return q.Where(col+" && ?::text[]", pq.Array(vals))
			}
		}
	case OpNEQ:
		if c.Value != nil {
			v := strings.TrimSpace(*c.Value)
			if v != "" {
				return q.Where("NOT ("+col+" @> ARRAY[?]::text[])", v)
			}
		}
	}
	return q
}

func applyIntOp(q *gorm.DB, col string, c Clause) *gorm.DB {
	switch c.Op {
	case OpEQ:
		if c.Value != nil {
			return q.Where(col+" = ?", *c.Value)
		}
	case OpIN:
		if len(c.Values) > 0 {
			return q.Where(col+" IN ?", c.Values)
		}
	}
	return q
}

func applyBoolOp(q *gorm.DB, col string, c Clause) *gorm.DB {
	if c.Value == nil {
		return q
	}
	v := strings.ToLower(strings.TrimSpace(*c.Value))
	if v == "true" {
		return q.Where(col+" = ?", true)
	}
	if v == "false" {
		return q.Where(col+" = ?", false)
	}
	return q
}

func applyDateOp(q *gorm.DB, col string, c Clause) (*gorm.DB, error) {
	now := time.Now()
	loc := now.Location()

	startOfDay := func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	endOfDay := func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 23, 59, 59, int(time.Second-time.Nanosecond), loc)
	}

	switch c.Op {
	case OpALLTIME:
		return q, nil
	case OpLAST7:
		s := startOfDay(now.AddDate(0, 0, -7))
		return q.Where(col+" >= ?", s), nil
	case OpLAST30:
		s := startOfDay(now.AddDate(0, 0, -30))
		return q.Where(col+" >= ?", s), nil
	case OpTHISMONTH:
		y, m, _ := now.Date()
		s := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		return q.Where(col+" >= ?", s), nil
	case OpLASTMONTH:
		y, m, _ := now.Date()
		firstThis := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		lastMonthEnd := firstThis.Add(-time.Nanosecond)
		s := time.Date(lastMonthEnd.Year(), lastMonthEnd.Month(), 1, 0, 0, 0, 0, loc)
		e := endOfDay(lastMonthEnd)
		return q.Where(col+" BETWEEN ? AND ?", s, e), nil
	case OpBETWEEN:
		if c.Start == nil || c.End == nil {
			return nil, fmt.Errorf("BETWEEN requires start and end")
		}
		s, err := time.ParseInLocation("2006-01-02", *c.Start, loc)
		if err != nil {
			return nil, fmt.Errorf("invalid start date")
		}
		e, err := time.ParseInLocation("2006-01-02", *c.End, loc)
		if err != nil {
			return nil, fmt.Errorf("invalid end date")
		}
		return q.Where(col+" BETWEEN ? AND ?", startOfDay(s), endOfDay(e)), nil
	}
	return q, nil
}

// optional helper if you need numeric checks elsewhere
func isNumeric(s string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil
}

// DownloadUpdates: builds Excel (yellow highlight) or CSV
func (as *AdminService) DownloadUpdates(mode Mode, clauses []Clause, format string) (contentType, filename string, out []byte, err error) {
	// 1) collect all matching request_ids using SAME search logic (clauses)
	requestIDs, err := as.collectAllRequestIDs(mode, clauses)
	if err != nil {
		return "", "", nil, err
	}

	// no matches -> empty file
	if len(requestIDs) == 0 {
		if format == "csv" {
			buf := &bytes.Buffer{}
			w := csv.NewWriter(buf)
			_ = w.Write([]string{"file_name", "file_id", "row_id", "request_ids", "changed_columns"})
			w.Flush()
			return "text/csv; charset=utf-8", "updates.csv", buf.Bytes(), nil
		}
		f := excelize.NewFile()
		f.SetSheetName("Sheet1", "Updates")
		_ = f.SetSheetRow("Updates", "A1", &[]interface{}{"file_name", "file_id", "row_id", "request_ids", "changed_columns"})
		b, _ := f.WriteToBuffer()
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "updates.xlsx", b.Bytes(), nil
	}

	// 2) fetch ALL details rows for those request_ids (single query)
	details, err := as.loadDetailsForRequests(requestIDs)
	if err != nil {
		return "", "", nil, err
	}
	if len(details) == 0 {
		// no details rows -> return empty
		if format == "csv" {
			buf := &bytes.Buffer{}
			w := csv.NewWriter(buf)
			_ = w.Write([]string{"file_name", "file_id", "row_id", "request_ids", "changed_columns"})
			w.Flush()
			return "text/csv; charset=utf-8", "updates.csv", buf.Bytes(), nil
		}
		f := excelize.NewFile()
		f.SetSheetName("Sheet1", "Updates")
		_ = f.SetSheetRow("Updates", "A1", &[]interface{}{"file_name", "file_id", "row_id", "request_ids", "changed_columns"})
		b, _ := f.WriteToBuffer()
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "updates.xlsx", b.Bytes(), nil
	}

	// 3) group updates by (file_id, row_id)
	type rowKey struct {
		FileID uint
		RowID  int
	}

	// rowUpdates[rowKey][field] = newValue (latest wins)
	rowUpdates := map[rowKey]map[string]string{}
	// changed fields set per row
	rowChanged := map[rowKey]map[string]struct{}{}
	// request IDs set per row
	rowReqIDs := map[rowKey]map[uint]struct{}{}
	// rowIDs per file
	rowIDsByFile := map[uint]map[int]struct{}{}

	// highlight rule: changed value => yellow
	shouldHighlight := func(oldV, newV string) bool {
		// If you want ONLY "newly added": return strings.TrimSpace(oldV)=="" && strings.TrimSpace(newV)!=""
		return strings.TrimSpace(oldV) != strings.TrimSpace(newV)
	}

	for _, d := range details {
		// Rejected detail rows are not applied to file_data, so skip them in exports too.
		if strings.EqualFold(strings.TrimSpace(d.Status), "rejected") {
			continue
		}

		rk := rowKey{FileID: d.FileID, RowID: d.RowID}

		if rowUpdates[rk] == nil {
			rowUpdates[rk] = map[string]string{}
		}
		if rowChanged[rk] == nil {
			rowChanged[rk] = map[string]struct{}{}
		}
		if rowReqIDs[rk] == nil {
			rowReqIDs[rk] = map[uint]struct{}{}
		}
		if rowIDsByFile[d.FileID] == nil {
			rowIDsByFile[d.FileID] = map[int]struct{}{}
		}

		rowUpdates[rk][d.FieldName] = d.NewValue

		if shouldHighlight(d.OldValue, d.NewValue) {
			rowChanged[rk][d.FieldName] = struct{}{}
		}
		rowReqIDs[rk][d.RequestID] = struct{}{}
		rowIDsByFile[d.FileID][d.RowID] = struct{}{}
	}

	// 4) load file metadata (filename + columns_order)
	fileIDs := make([]uint, 0, len(rowIDsByFile))
	for fid := range rowIDsByFile {
		fileIDs = append(fileIDs, fid)
	}
	sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })

	files, err := as.loadFiles(fileIDs)
	if err != nil {
		return "", "", nil, err
	}
	fileByID := map[uint]fileMeta{}
	for _, fm := range files {
		fileByID[fm.ID] = fm
	}

	// 5) resolve latest version per file + fetch only needed rows from file_data
	latestVersion := map[uint]int{}
	resolveLatest := func(fileID uint) (int, error) {
		if v, ok := latestVersion[fileID]; ok {
			return v, nil
		}
		var v int
		if err := as.DB.Table("file_data").
			Select("COALESCE(MAX(version), 0)").
			Where("file_id = ?", fileID).
			Scan(&v).Error; err != nil {
			return 0, err
		}
		latestVersion[fileID] = v
		return v, nil
	}

	// fileDataByRow[fileID][rowID] = rowData JSON map
	fileDataByRow := map[uint]map[int]map[string]any{}

	for _, fid := range fileIDs {
		version, err := resolveLatest(fid)
		if err != nil {
			return "", "", nil, err
		}

		// row ids
		rset := rowIDsByFile[fid]
		rowIDs := make([]uint, 0, len(rset))
		for rid := range rset {
			if rid > 0 {
				rowIDs = append(rowIDs, uint(rid))
			}
		}
		sort.Slice(rowIDs, func(i, j int) bool { return rowIDs[i] < rowIDs[j] })

		var rows []fileDataRow
		if err := as.DB.Table("file_data").
			Select("id, file_id, version, row_data").
			Where("file_id = ? AND version = ? AND id IN ?", fid, version, rowIDs).
			Find(&rows).Error; err != nil {
			return "", "", nil, err
		}

		if fileDataByRow[fid] == nil {
			fileDataByRow[fid] = map[int]map[string]any{}
		}

		for _, r := range rows {
			base := map[string]any{}
			if len(r.RowData) > 0 {
				_ = json.Unmarshal(r.RowData, &base)
			}
			fileDataByRow[fid][int(r.ID)] = base
		}
	}

	// 6) Build export structure per file

	byFile := map[uint][]exportRow{}

	for rk, updates := range rowUpdates {
		fm := fileByID[rk.FileID]
		version := latestVersion[rk.FileID] // already resolved above

		cols := []string{}
		_ = json.Unmarshal(fm.ColumnsOrder, &cols)

		// base row map from file_data (may be missing => empty)
		base := map[string]any{}
		if m := fileDataByRow[rk.FileID]; m != nil {
			if rowMap := m[rk.RowID]; rowMap != nil {
				for k, v := range rowMap {
					base[k] = v
				}
			}
		}

		// apply updates (new_value overwrites)
		for field, newV := range updates {
			base[field] = newV
		}

		// changed columns list
		chset := rowChanged[rk]
		changedCols := make([]string, 0, len(chset))
		for c := range chset {
			changedCols = append(changedCols, c)
		}
		sort.Strings(changedCols)

		// request ids list
		reqSet := rowReqIDs[rk]
		reqIDs := make([]uint, 0, len(reqSet))
		for id := range reqSet {
			reqIDs = append(reqIDs, id)
		}
		sort.Slice(reqIDs, func(i, j int) bool { return reqIDs[i] < reqIDs[j] })

		byFile[rk.FileID] = append(byFile[rk.FileID], exportRow{
			FileID:         rk.FileID,
			FileName:       fm.Filename,
			Version:        version,
			RowID:          rk.RowID,
			RequestIDs:     reqIDs,
			ChangedColumns: changedCols,
			ChangedSet:     chset,
			ColumnsOrder:   cols,
			ValuesByCol:    base,
		})
	}

	// sort rows in each file
	for fid := range byFile {
		sort.Slice(byFile[fid], func(i, j int) bool { return byFile[fid][i].RowID < byFile[fid][j].RowID })
	}

	// 7) Output
	ts := time.Now().Format("20060102_150405")
	if format == "csv" {
		b, err := buildCSV(byFile)
		if err != nil {
			return "", "", nil, err
		}
		return "text/csv; charset=utf-8", fmt.Sprintf("updates_%s.csv", ts), b, nil
	}

	b, err := buildXLSX(byFile)
	if err != nil {
		return "", "", nil, err
	}
	return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", fmt.Sprintf("updates_%s.xlsx", ts), b, nil
}

// ---- DB helpers ----

func (as *AdminService) loadDetailsForRequests(requestIDs []uint) ([]fileEditRequestDetailsRow, error) {
	var rows []fileEditRequestDetailsRow
	err := as.DB.Table("file_edit_request_details").
		Select("id, request_id, file_id, filename, row_id, field_name, old_value, new_value, COALESCE(status, 'pending') AS status, COALESCE(reviewer_comment, '') AS reviewer_comment, created_at").
		Where("request_id IN ?", requestIDs).
		Order("file_id ASC, row_id ASC, id ASC").
		Scan(&rows).Error
	return rows, err
}

func (as *AdminService) loadFiles(fileIDs []uint) ([]fileMeta, error) {
	var files []fileMeta
	err := as.DB.Table("file").
		Select("id, filename, columns_order").
		Where("id IN ?", fileIDs).
		Scan(&files).Error
	return files, err
}

func decodeAdminSearchRows(data any) ([]adminSearchRow, error) {
	// Works whether data is []map[string]any, []any, or even already typed structs
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal search data: %w", err)
	}

	var rows []adminSearchRow
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("failed to unmarshal search data rows: %w", err)
	}
	return rows, nil
}

func (as *AdminService) collectAllRequestIDs(mode Mode, clauses []Clause) ([]uint, error) {
	page := 1
	pageSize := 200

	all := []uint{}

	for {
		req := AdminFileEditSearchRequest{
			Mode:     mode,
			Clauses:  clauses,
			Page:     page,
			PageSize: pageSize,
		}

		resp, err := as.SearchFileEditRequests(req)
		if err != nil {
			return nil, err
		}

		rows, err := decodeAdminSearchRows(resp.Data)
		if err != nil {
			return nil, err
		}

		for _, r := range rows {
			if r.RequestID > 0 {
				all = append(all, r.RequestID)
			}
		}

		if resp.TotalPages <= 1 || page >= resp.TotalPages {
			break
		}
		page++
	}

	// de-dup
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	uniq := all[:0]
	var last uint
	for i, v := range all {
		if i == 0 || v != last {
			uniq = append(uniq, v)
			last = v
		}
	}

	return uniq, nil
}

func buildCSV(byFile map[uint][]exportRow) ([]byte, error) {
	buf := &bytes.Buffer{}
	w := csv.NewWriter(buf)

	// build a global union of columns across files (keeps first-seen order)
	globalCols := []string{}
	seen := map[string]struct{}{}

	// stable order by file_id
	fileIDs := make([]uint, 0, len(byFile))
	for fid := range byFile {
		fileIDs = append(fileIDs, fid)
	}
	sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })

	for _, fid := range fileIDs {
		rows := byFile[fid]
		if len(rows) == 0 {
			continue
		}
		for _, c := range rows[0].ColumnsOrder {
			if _, ok := seen[c]; !ok {
				seen[c] = struct{}{}
				globalCols = append(globalCols, c)
			}
		}
	}

	header := []string{"file_name", "file_id", "version", "row_id", "request_ids", "changed_columns"}
	header = append(header, globalCols...)
	if err := w.Write(header); err != nil {
		return nil, err
	}

	for _, fid := range fileIDs {
		for _, r := range byFile[fid] {
			reqIDs := make([]string, 0, len(r.RequestIDs))
			for _, id := range r.RequestIDs {
				reqIDs = append(reqIDs, fmt.Sprintf("%d", id))
			}

			rec := []string{
				r.FileName,
				fmt.Sprintf("%d", r.FileID),
				fmt.Sprintf("%d", r.Version),
				fmt.Sprintf("%d", r.RowID),
				strings.Join(reqIDs, ","),
				strings.Join(r.ChangedColumns, ","),
			}

			for _, c := range globalCols {
				v := r.ValuesByCol[c]
				if v == nil {
					rec = append(rec, "")
				} else {
					rec = append(rec, fmt.Sprintf("%v", v))
				}
			}

			if err := w.Write(rec); err != nil {
				return nil, err
			}
		}
	}

	w.Flush()
	return buf.Bytes(), w.Error()
}

// ---- XLSX (yellow highlight on changed cells) ----

func buildXLSX(byFile map[uint][]exportRow) ([]byte, error) {
	f := excelize.NewFile()

	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#E2E8F0"}},
	})

	highlightStyle, _ := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#FFFF99"}},
	})

	defaultSheet := f.GetSheetName(0)

	// stable file order
	fileIDs := make([]uint, 0, len(byFile))
	for fid := range byFile {
		fileIDs = append(fileIDs, fid)
	}
	sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })

	used := map[string]int{}

	for _, fid := range fileIDs {
		rows := byFile[fid]
		if len(rows) == 0 {
			continue
		}

		sheet := safeSheetName(rows[0].FileName)
		if sheet == "" {
			sheet = fmt.Sprintf("File_%d", fid)
		}
		if n, ok := used[sheet]; ok {
			n++
			used[sheet] = n
			sheet = fmt.Sprintf("%s_%d", sheet, n)
		} else {
			used[sheet] = 1
		}

		f.NewSheet(sheet)

		sw, err := f.NewStreamWriter(sheet)
		if err != nil {
			return nil, err
		}

		cols := rows[0].ColumnsOrder

		// header row
		header := []interface{}{
			excelize.Cell{Value: "file_name", StyleID: headerStyle},
			excelize.Cell{Value: "file_id", StyleID: headerStyle},
			excelize.Cell{Value: "version", StyleID: headerStyle},
			excelize.Cell{Value: "row_id", StyleID: headerStyle},
			excelize.Cell{Value: "request_ids", StyleID: headerStyle},
			excelize.Cell{Value: "changed_columns", StyleID: headerStyle},
		}
		for _, c := range cols {
			header = append(header, excelize.Cell{Value: c, StyleID: headerStyle})
		}
		_ = sw.SetRow("A1", header)

		rowNum := 2
		for _, r := range rows {
			reqIDs := make([]string, 0, len(r.RequestIDs))
			for _, id := range r.RequestIDs {
				reqIDs = append(reqIDs, fmt.Sprintf("%d", id))
			}

			values := []interface{}{
				r.FileName,
				r.FileID,
				r.Version,
				r.RowID,
				strings.Join(reqIDs, ","),
				strings.Join(r.ChangedColumns, ","),
			}

			for _, c := range cols {
				v := r.ValuesByCol[c]
				if _, changed := r.ChangedSet[c]; changed {
					values = append(values, excelize.Cell{Value: fmt.Sprintf("%v", v), StyleID: highlightStyle})
				} else {
					if v == nil {
						values = append(values, "")
					} else {
						values = append(values, fmt.Sprintf("%v", v))
					}
				}
			}

			cell, _ := excelize.CoordinatesToCellName(1, rowNum)
			_ = sw.SetRow(cell, values)
			rowNum++
		}

		if err := sw.Flush(); err != nil {
			return nil, err
		}
	}

	if defaultSheet != "" {
		f.DeleteSheet(defaultSheet)
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func safeSheetName(name string) string {
	n := strings.TrimSpace(name)
	n = strings.NewReplacer(":", "_", "\\", "_", "/", "_", "?", "_", "*", "_", "[", "_", "]", "_").Replace(n)
	if len(n) > 31 {
		n = n[:31]
	}
	return n
}

func (as *AdminService) StreamMediaZip(ctx context.Context, out io.Writer, req AdminDownloadMediaRequest) error {
	// 1) resolve requestIDs
	requestIDs := dedupeAndFilterRequestIDs(req.RequestIDs)

	// If request_ids not present/empty, fallback to clauses
	if len(requestIDs) == 0 {
		ids, err := as.collectAllRequestIDs(ModeChanges, req.Clauses)
		if err != nil {
			return err
		}
		requestIDs = dedupeAndFilterRequestIDs(ids)
	}

	if len(requestIDs) == 0 {
		return fmt.Errorf("no matching requests found")
	}

	// 2) load media rows
	rows, err := as.loadMediaRows(requestIDs, req.DocumentType, req.OnlyApproved)
	if err != nil {
		return err
	}

	// The database query stops as soon as it proves this request is too large,
	// keeping metadata memory bounded for broad admin filters.
	if len(rows) > maxMediaZipFiles {
		return fmt.Errorf("too many files to zip (limit %d). narrow your filters", maxMediaZipFiles)
	}
	rows = dedupeMediaZipRows(rows)
	if len(rows) == 0 {
		return fmt.Errorf("no media found for the selected filters")
	}

	// stable ordering
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].UserID != rows[j].UserID {
			return rows[i].UserID < rows[j].UserID
		}
		if rows[i].DocumentType != rows[j].DocumentType {
			return rows[i].DocumentType < rows[j].DocumentType
		}
		if rows[i].RequestID != rows[j].RequestID {
			return rows[i].RequestID < rows[j].RequestID
		}
		return rows[i].ID < rows[j].ID
	})

	// 3) init GCS client once
	client, err := newGCSClientHook(ctx)

	if err != nil {
		return err
	}
	defer client.Close()

	// 4) zip writer (streaming)
	zw := zip.NewWriter(out)
	defer zw.Close()

	envBucket := strings.TrimSpace(os.Getenv("BUCKET_NAME"))

	created := 0
	usedEntryNames := make(map[string]int, len(rows))

	for _, r := range rows {
		entryPath := buildZipEntryPath(r, req.CategorizeByUser, req.CategorizeByType)

		baseName := ""
		if strings.TrimSpace(r.FileName) != "" {
			baseName = sanitizeFilename(r.FileName)
		}
		if baseName == "" {
			_, obj, _ := parseGSURLAdmin(r.ObjectURL)
			baseName = sanitizeFilename(path.Base(obj))
			if baseName == "" {
				baseName = fmt.Sprintf("file_%d", r.ID)
			}
		}

		finalName := uniqueZipEntryName(entryPath, baseName, usedEntryNames)
		zipFullPath := entryPath + finalName

		w, err := zw.Create(zipFullPath)
		if err != nil {
			return err
		}

		bucket, objectPath, err := parseGSURLAdmin(r.ObjectURL)
		if err != nil {
			return err
		}
		if bucket == "" {
			bucket = envBucket
		}
		if bucket == "" {
			return fmt.Errorf("bucket name not found for id=%d (gs url + BUCKET_NAME empty)", r.ID)
		}

		rc, err := client.Bucket(bucket).Object(objectPath).NewReader(ctx)
		if err != nil {
			return err
		}

		_, copyErr := io.Copy(w, rc)
		_ = rc.Close()

		if copyErr != nil {
			return copyErr
		}

		created++
	}

	if created == 0 {
		return fmt.Errorf("no files zipped")
	}

	return nil
}

func (as *AdminService) loadMediaRows(requestIDs []uint, docType string, onlyApproved *bool) ([]mediaZipRow, error) {
	docType = strings.ToLower(strings.TrimSpace(docType))
	if docType == "" {
		docType = "all"
	}
	if docType != "all" && docType != "photos" && docType != "document" && docType != "stories" {
		return nil, fmt.Errorf("invalid document_type: %s (use all/photos/document/stories)", docType)
	}

	var q *gorm.DB
	statusColumn := "p.status"
	if docType == "stories" {
		statusColumn = "s.status"
		q = as.DB.Table("file_row_achiever_stories s").
			Joins("JOIN file_edit_request r ON r.request_id = s.request_id").
			Select(`
				s.id,
				s.request_id,
				s.row_id,
				s.story_url AS photo_url,
				COALESCE(s.file_name,'') AS file_name,
				'stories' AS document_type,
				COALESCE(s.story_type,'') AS document_category,
				r.user_id AS user_id,
				COALESCE(r.firstname,'') AS user_first,
				COALESCE(r.lastname,'') AS user_last
			`).
			Where("s.request_id IN ?", requestIDs).
			Where("COALESCE(s.story_url, '') <> ''")
	} else {
		q = as.DB.Table("file_edit_request_photos p").
			Joins("JOIN file_edit_request r ON r.request_id = p.request_id").
			Select(`
				p.id,
				p.request_id,
				p.row_id,
				p.photo_url,
				COALESCE(p.file_name,'') AS file_name,
				COALESCE(p.document_type,'') AS document_type,
				COALESCE(p.document_category,'') AS document_category,
				r.user_id AS user_id,
				COALESCE(r.firstname,'') AS user_first,
				COALESCE(r.lastname,'') AS user_last
			`).
			Where("p.request_id IN ?", requestIDs)

		if docType != "all" {
			q = q.Where("p.document_type = ?", docType)
		}
	}

	if onlyApproved != nil {
		if *onlyApproved {
			q = q.Where(statusColumn+" = ?", "approved")
		} else {
			q = q.Where(statusColumn+" <> ?", "approved")
		}
	}

	var out []mediaZipRow
	if err := q.
		Limit(maxMediaZipFiles + 1).
		Scan(&out).Error; err != nil {
		return nil, err
	}

	return out, nil
}

func dedupeMediaZipRows(rows []mediaZipRow) []mediaZipRow {
	if len(rows) < 2 {
		return rows
	}

	seen := make(map[string]struct{}, len(rows))
	out := make([]mediaZipRow, 0, len(rows))
	for _, row := range rows {
		keyParts := []string{
			strings.ToLower(strings.TrimSpace(row.DocumentType)),
			strings.TrimSpace(row.ObjectURL),
		}
		if keyParts[1] == "" {
			keyParts = append(keyParts, strings.TrimSpace(row.FileName))
		}
		key := strings.Join(keyParts, "|")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, row)
	}
	return out
}

// Folder logic based on flags
func buildZipEntryPath(r mediaZipRow, byUser bool, byType bool) string {
	parts := []string{}

	if byUser {
		userFolder := fmt.Sprintf("user_%d", r.UserID)
		fullName := strings.TrimSpace(strings.Join([]string{r.UserFirst, r.UserLast}, " "))
		if fullName != "" {
			userFolder = fmt.Sprintf("user_%d_%s", r.UserID, sanitizePathPart(fullName))
		}
		parts = append(parts, userFolder)
	}

	if byType {
		switch r.DocumentType {
		case "photos":
			parts = append(parts, "photos")
		case "stories":
			parts = append(parts, "stories")
		default:
			parts = append(parts, "documents")
		}
	}

	if len(parts) == 0 {
		return "" // flat zip
	}

	return strings.Join(parts, "/") + "/"
}

func parseGSURLAdmin(gsURL string) (bucket string, objectPath string, err error) {
	gsURL = strings.TrimSpace(gsURL)
	if gsURL == "" {
		return "", "", fmt.Errorf("empty gs url")
	}
	if !strings.HasPrefix(gsURL, "gs://") {
		return "", "", fmt.Errorf("invalid gs url (must start with gs://): %s", gsURL)
	}

	rest := strings.TrimPrefix(gsURL, "gs://") // bucket/object
	slash := strings.Index(rest, "/")
	if slash < 0 || slash == len(rest)-1 {
		return "", "", fmt.Errorf("invalid gs url format: %s", gsURL)
	}

	bucket = rest[:slash]
	objectPath = rest[slash+1:]
	if strings.TrimSpace(objectPath) == "" {
		return "", "", fmt.Errorf("empty object path in gs url: %s", gsURL)
	}
	return bucket, objectPath, nil
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "file"
	}
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "..", "_")
	name = strings.ReplaceAll(name, `"`, "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	return name
}

func sanitizePathPart(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	// keep it simple: replace separators and weird chars
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, "*", "_")
	s = strings.ReplaceAll(s, "?", "_")
	s = strings.ReplaceAll(s, `"`, "")
	s = strings.ReplaceAll(s, "<", "_")
	s = strings.ReplaceAll(s, ">", "_")
	s = strings.ReplaceAll(s, "|", "_")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

func uniqueZipEntryName(entryPath string, baseName string, used map[string]int) string {
	baseName = sanitizeFilename(baseName)
	if baseName == "" {
		baseName = "file"
	}

	key := strings.ToLower(entryPath + baseName)
	if used[key] == 0 {
		used[key] = 1
		return baseName
	}

	ext := path.Ext(baseName)
	stem := strings.TrimSuffix(baseName, ext)
	for n := used[key] + 1; ; n++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		candidateKey := strings.ToLower(entryPath + candidate)
		if used[candidateKey] != 0 {
			continue
		}
		used[key] = n
		used[candidateKey] = 1
		return candidate
	}
}
