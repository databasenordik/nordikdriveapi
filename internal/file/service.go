package file

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/honour"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/util"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/iancoleman/orderedmap"
	"github.com/xuri/excelize/v2"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type FileService struct {
	DB     *gorm.DB
	Mailer mailer.EmailSender
}

type fileVersionHistoryQueryRow struct {
	FileVersionWithUser
	InsertedBy uint `gorm:"column:inserted_by"`
}

type fileDataVersionHistoryAggregate struct {
	Version      int    `gorm:"column:version"`
	Rows         int64  `gorm:"column:rows"`
	CreatedAtRaw string `gorm:"column:created_at_raw"`
	InsertedBy   uint   `gorm:"column:inserted_by"`
}

type historyUserName struct {
	ID        uint   `gorm:"column:id"`
	Firstname string `gorm:"column:firstname"`
	Lastname  string `gorm:"column:lastname"`
}

const (
	defaultExcelImportShortDatePattern = "yyyy-mm-dd"
	excelShortDatePatternEnv           = "EXCEL_SHORT_DATE_PATTERN"
)

var (
	uploadToGCSHook   = util.UploadPhotoToGCS
	moveGCSFolderHook = util.MoveGCSFolder

	newGCSClientHook = func(ctx context.Context) (*storage.Client, error) {
		return storage.NewClient(ctx)
	}
)

const (
	fileEditRequestTypeEdit          = "edit"
	fileEditRequestTypeAchieverStory = "achiever_story"
)

func (fs *FileService) SaveFilesMultipart(uploadedFiles []*multipart.FileHeader, filenames FileUploadInput, userID uint) ([]File, error) {
	if err := ensureFileDescriptionSchema(fs.DB); err != nil {
		return nil, err
	}
	if err := validateUploadInputCounts(len(uploadedFiles), filenames); err != nil {
		return nil, err
	}

	var savedFiles []File
	files := filenames.FileNames
	privateList := filenames.Private
	communityFilterList := filenames.CommunityFilter

	for i, fileHeader := range uploadedFiles {
		filename := files[i]
		description := normalizeUploadDescription(filenames.Descriptions, i)
		private := privateList[i]
		communityFilter := communityFilterList[i]

		// Check for duplicate filename
		var existing File
		if err := fs.DB.Where("filename = ?", filename).First(&existing).Error; err == nil {
			return nil, fmt.Errorf("file with name %s already exists", filename)
		}

		f, err := fileHeader.Open()
		if err != nil {
			return nil, err
		}
		defer f.Close()

		ext := filepath.Ext(fileHeader.Filename)
		var headers []string
		var dataRows [][]string

		if ext == ".xlsx" || ext == ".xls" {
			headers, dataRows, err = parseExcelReader(f)
		} else if ext == ".csv" {
			headers, dataRows, err = parseCSVReader(f)
		} else {
			return nil, fmt.Errorf("unsupported file type: %s", ext)
		}
		if err != nil {
			return nil, err
		}

		// Save File entry with column order
		headersJSON, err := marshalColumnsOrder(headers)
		if err != nil {
			return nil, err
		}

		newFile := File{
			Filename:        filename,
			Description:     description,
			InsertedBy:      userID,
			CreatedAt:       time.Now(),
			Private:         private,
			Version:         1,
			IsDelete:        false,
			Rows:            len(dataRows),
			Size:            float64(fileHeader.Size) / 1024.0,
			ColumnsOrder:    headersJSON, // store headers
			CommunityFilter: communityFilter,
		}
		if err := fs.DB.Create(&newFile).Error; err != nil {
			return nil, err
		}

		// Save FileVersion entry
		fileVersion := FileVersion{
			FileID:       newFile.ID,
			Filename:     filename,
			Description:  description,
			InsertedBy:   userID,
			CreatedAt:    time.Now(),
			Private:      private,
			Version:      1,
			IsDelete:     false,
			Rows:         len(dataRows),
			Size:         float64(fileHeader.Size) / 1024.0,
			ColumnsOrder: headersJSON,
		}
		if err := fs.DB.Create(&fileVersion).Error; err != nil {
			return nil, err
		}

		// Save each row as normal JSON
		for _, row := range dataRows {
			rowMap := make(map[string]string)
			for j, header := range headers {
				val := ""
				if j < len(row) {
					val = row[j]
				}
				rowMap[header] = val
			}

			jsonBytes, _ := json.Marshal(rowMap)

			record := FileData{
				FileID:     newFile.ID,
				RowData:    datatypes.JSON(jsonBytes),
				InsertedBy: userID,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
				Version:    1,
			}

			if err := fs.DB.Create(&record).Error; err != nil {
				return nil, err
			}
		}

		savedFiles = append(savedFiles, newFile)
	}

	return savedFiles, nil
}

// parseExcelReader preserves original Excel column order
func parseExcelReader(file multipart.File) ([]string, [][]string, error) {
	defer file.Seek(0, 0)

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(file); err != nil {
		return nil, nil, fmt.Errorf("failed to read excel file: %w", err)
	}

	// Force Excel's locale-sensitive short-date cells (for example built-in
	// numFmt 14 => mm-dd-yy) into a stable import format so centuries don't get
	// collapsed during upload.
	f, err := excelize.OpenReader(bytes.NewReader(buf.Bytes()), excelize.Options{
		ShortDatePattern: excelImportShortDatePattern(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse excel file: %w", err)
	}

	sheetName := f.GetSheetName(0)
	rows, err := f.GetRows(sheetName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read rows: %w", err)
	}
	if len(rows) < 1 {
		return nil, nil, fmt.Errorf("excel file is empty")
	}

	headers := rows[0]
	var dataRows [][]string
	colorToSource := map[string]string{
		"#FFC000": "NCTR SOURCE",
		"#0070C0": "Office of the Registrar General",
		"#00B050": "REPORT COMPLETE",
		"#00B0F0": "Library and Archives Canada",
		"#92D050": "St. Margaret's Register & OLOL List",
		"#9F5FCF": "CIRNAC SOURCE",
		"#FF0000": "CORONER'S OFFICE SOURCE",
		"#FFCCCC": "STUDENTS REMOVED FROM NCTR",
		"#FFFF99": "FURTHER INVESTIGATION REQUIRED",
	}

	for rowIdx, _ := range rows[1:] {
		newRow := make([]string, len(headers))
		for colIdx := range headers {
			cellRef, _ := excelize.CoordinatesToCellName(colIdx+1, rowIdx+2)
			val, _ := f.GetCellValue(sheetName, cellRef)
			if styleID, err := f.GetCellStyle(sheetName, cellRef); err == nil && styleID != 0 {
				if style, err := f.GetStyle(styleID); err == nil && style != nil {
					if len(style.Fill.Color) > 0 {
						rawColor := style.Fill.Color[0]
						hex := normalizeColorHex(rawColor)
						if src, ok := colorToSource[hex]; ok && val != "" {
							// Append source in brackets (no duplicate-avoid logic per request)
							val = fmt.Sprintf("%s (%s)", val, src)
						}
					}
				}
			}
			newRow[colIdx] = val
		}
		dataRows = append(dataRows, newRow)
	}

	return headers, dataRows, nil
}

func excelImportShortDatePattern() string {
	pattern := strings.TrimSpace(os.Getenv(excelShortDatePatternEnv))
	if pattern == "" {
		return defaultExcelImportShortDatePattern
	}
	return pattern
}

func marshalColumnsOrder(headers []string) (datatypes.JSON, error) {
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal columns order: %w", err)
	}
	return datatypes.JSON(headersJSON), nil
}

func unmarshalColumnsOrder(columnsOrder datatypes.JSON) ([]string, error) {
	if len(bytes.TrimSpace(columnsOrder)) == 0 {
		return nil, nil
	}

	var parsed []string
	if err := json.Unmarshal(columnsOrder, &parsed); err != nil {
		return nil, fmt.Errorf("failed to unmarshal columns order: %w", err)
	}
	return parsed, nil
}

func (fs *FileService) getColumnsOrderForVersion(file File, version int) ([]string, error) {
	columnsOrderJSON := file.ColumnsOrder

	if version != file.Version {
		var fileVersion FileVersion
		err := preferredFileVersionQuery(fs.DB, file.ID, version).
			Select("columns_order").
			First(&fileVersion).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		if err == nil && len(bytes.TrimSpace(fileVersion.ColumnsOrder)) > 0 {
			columnsOrderJSON = fileVersion.ColumnsOrder
		}
	}

	return unmarshalColumnsOrder(columnsOrderJSON)
}

func (fs *FileService) ReplaceFiles(uploadedFile *multipart.FileHeader, fileID uint, userID uint) error {
	if err := ensureFileDescriptionSchema(fs.DB); err != nil {
		return err
	}

	var existing File
	if err := fs.DB.First(&existing, fileID).Error; err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	if err := fs.ensureNoOpenRequestsForFile(existing, "replace"); err != nil {
		return err
	}
	if err := fs.ensureFileVersionTransitionIdle(existing); err != nil {
		return err
	}

	f, err := uploadedFile.Open()
	if err != nil {
		return err
	}
	defer f.Close()

	// 3. Parse file
	ext := filepath.Ext(uploadedFile.Filename)
	var headers []string
	var dataRows [][]string

	if ext == ".xlsx" || ext == ".xls" {
		headers, dataRows, err = parseExcelReader(f)
	} else if ext == ".csv" {
		headers, dataRows, err = parseCSVReader(f)
	} else {
		return fmt.Errorf("unsupported file type: %s", ext)
	}
	if err != nil {
		return err
	}

	headersJSON, err := marshalColumnsOrder(headers)
	if err != nil {
		return err
	}

	sizeInBytes := uploadedFile.Size
	sizeInKB := float64(sizeInBytes) / 1024.0

	return fs.DB.Transaction(func(tx *gorm.DB) error {
		lockedFile, err := lockFileForVersionTransition(tx, existing.ID)
		if err != nil {
			return err
		}
		if err := ensureNoOpenRequestsForFileWithDB(tx, lockedFile, "replace"); err != nil {
			return err
		}
		if err := ensureFileVersionTransitionIdleWithDB(tx, lockedFile.ID); err != nil {
			var transitionErr *FileVersionTransitionError
			if errors.As(err, &transitionErr) {
				transitionErr.Filename = lockedFile.Filename
			}
			return err
		}

		newVersion, err := nextFileVersionNumber(tx, lockedFile.ID, lockedFile.Version)
		if err != nil {
			return err
		}

		fileVersion := FileVersion{
			FileID:               lockedFile.ID,
			Filename:             lockedFile.Filename,
			Description:          lockedFile.Description,
			InsertedBy:           userID,
			CreatedAt:            time.Now(),
			Private:              lockedFile.Private,
			Version:              newVersion,
			IsDelete:             false,
			Rows:                 len(dataRows),
			Size:                 sizeInKB,
			ColumnsOrder:         headersJSON,
			ReconciliationStatus: fileVersionStatusProcessing,
			ReconciliationError:  "",
			TransitionOperation:  versionTransitionOperationReplace,
		}
		if err := tx.Create(&fileVersion).Error; err != nil {
			return err
		}

		for _, row := range dataRows {
			recordMap := make(map[string]string)
			for j, header := range headers {
				if j < len(row) {
					recordMap[header] = row[j]
				} else {
					recordMap[header] = ""
				}
			}

			jsonBytes, err := json.Marshal(recordMap)
			if err != nil {
				return err
			}

			record := FileData{
				FileID:     lockedFile.ID,
				RowData:    jsonBytes,
				InsertedBy: userID,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
				Version:    newVersion,
			}
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
		}

		if err := honour.ClearForFileTx(tx, lockedFile.ID); err != nil {
			return err
		}

		return fs.enqueueVersionTransitionJob(
			tx,
			lockedFile.ID,
			fileVersion.ID,
			lockedFile.Version,
			newVersion,
			newVersion,
			userID,
			versionTransitionOperationReplace,
		)
	})
}

func (fs *FileService) GetUserRole(userID uint) (string, error) {
	var user auth.Auth
	if err := fs.DB.First(&user, userID).Error; err != nil {
		return "", err
	}
	return user.Role, nil
}

func (fs *FileService) GetAllFiles(userID uint, role string) ([]FileWithUser, error) {
	var files []FileWithUser

	if role == "Admin" {
		// Admin → all files with uploader info
		if err := fs.DB.
			Table("file f").
			Select("f.*, u.firstname, u.lastname").
			Joins("LEFT JOIN users u ON u.id = f.inserted_by").
			Scan(&files).Error; err != nil {
			return nil, err
		}
		return files, nil
	}

	// User → public files OR private files they have access to
	err := fs.DB.
		Raw(`
			SELECT f.*, u.firstname, u.lastname
			FROM file f
			LEFT JOIN users u ON u.id = f.inserted_by
			LEFT JOIN file_access fa ON f.id = fa.file_id AND fa.user_id = ?
			WHERE (f.private = false OR fa.user_id = ?) AND f.is_delete = ?
		`, userID, userID, false).
		Scan(&files).Error

	if err != nil {
		return nil, err
	}

	return files, nil
}

func (fs *FileService) GetFileData(filename string, version int) ([]FileData, error) {
	var file File

	// Fetch file by filename
	if err := fs.DB.Where("filename = ?", filename).First(&file).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}

	if file.Version <= 0 {
		if version > 0 {
			file.Version = version
		} else {
			file.Version = 1
		}
	}

	if version <= 0 {
		version = file.Version
	}

	if version != file.Version {
		var requestedVersion FileVersion
		selectClause := "version"
		if fs.DB.Migrator().HasTable(&FileVersion{}) && fs.DB.Migrator().HasColumn(&FileVersion{}, "reconciliation_status") {
			selectClause = "version, reconciliation_status"
		}

		if err := preferredFileVersionQuery(fs.DB, file.ID, version).
			Select(selectClause).
			First(&requestedVersion).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, nil
			}
			return nil, err
		}
		if requestedVersion.ReconciliationStatus != "" && requestedVersion.ReconciliationStatus != fileVersionStatusReady {
			return nil, fmt.Errorf("version %d is still being reconciled", version)
		}
	}

	var fileData []FileData
	if err := fs.DB.Where("file_id = ? AND version = ?", file.ID, version).
		Order("id ASC"). // preserve row insertion order
		Find(&fileData).Error; err != nil {
		return nil, err
	}

	columnsOrder, err := fs.getColumnsOrderForVersion(file, version)
	if err != nil {
		return nil, err
	}

	// Reorder each row's JSON according to ColumnsOrder
	for i := range fileData {
		if len(columnsOrder) == 0 {
			continue
		}

		var rowMap map[string]interface{}
		if err := json.Unmarshal(fileData[i].RowData, &rowMap); err != nil {
			return nil, fmt.Errorf("failed to unmarshal row: %w", err)
		}

		orderedRow := orderedmap.New()
		for _, col := range columnsOrder {
			if val, exists := rowMap[col]; exists {
				orderedRow.Set(col, val)
			} else {
				orderedRow.Set(col, "") // fill missing columns
			}
		}

		// Column order controls presentation, not which stored fields are returned.
		// Preserve fields added after the saved schema so configuration-driven
		// views and edit forms can still access their values.
		orderedColumns := make(map[string]struct{}, len(columnsOrder))
		for _, col := range columnsOrder {
			orderedColumns[col] = struct{}{}
		}
		extraColumns := make([]string, 0)
		for col := range rowMap {
			if _, exists := orderedColumns[col]; !exists {
				extraColumns = append(extraColumns, col)
			}
		}
		sort.Strings(extraColumns)
		for _, col := range extraColumns {
			orderedRow.Set(col, rowMap[col])
		}

		// Marshal back to JSON
		jsonBytes, err := orderedRow.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("failed to marshal ordered row: %w", err)
		}

		fileData[i].RowData = datatypes.JSON(jsonBytes)
	}

	return fileData, nil
}

func (fs *FileService) GetNormalizedFileData(filename string, version int) ([]FileDataNormalized, error) {
	if err := ensureNormalizationSchema(fs.DB); err != nil {
		return nil, err
	}

	if strings.TrimSpace(filename) == "" {
		return nil, nil
	}

	var file File
	if err := fs.DB.Where("filename = ? AND is_delete = ?", filename, false).First(&file).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}

	if version <= 0 {
		version = file.Version
	}

	var normalizedRows []FileDataNormalized
	if err := fs.DB.Where("file_id = ? AND version = ?", file.ID, version).
		Order("source_row_id ASC").
		Find(&normalizedRows).Error; err != nil {
		return nil, err
	}

	return normalizedRows, nil
}

func (fs *FileService) SyncNormalizedFileData(filename string, version int) (*NormalizationSyncResult, error) {
	options := NormalizationSyncOptions{
		BatchSize: defaultNormalizationBatch,
	}

	if strings.TrimSpace(filename) != "" {
		var file File
		if err := fs.DB.Where("filename = ? AND is_delete = ?", filename, false).First(&file).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, nil
			}
			return nil, err
		}

		options.FileID = &file.ID
		if version <= 0 {
			version = file.Version
		}
	}

	if version > 0 {
		versionCopy := version
		options.Version = &versionCopy
	}

	return RunNormalizationSync(fs.DB, options)
}

// ...existing code...
// normalizeColorHex converts excelize color strings (ARGB/RGB/short) into "#RRGGBB" uppercase.
func normalizeColorHex(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// remove common prefixes
	raw = strings.TrimPrefix(raw, "0x")
	raw = strings.TrimPrefix(raw, "#")
	raw = strings.ToUpper(raw)

	// If ARGB (8 chars) drop the leading alpha (first two chars)
	if len(raw) == 8 {
		raw = raw[2:]
	}
	// If 6 chars -> use as is
	if len(raw) == 6 {
		return "#" + raw
	}
	// If 3 chars -> expand to 6
	if len(raw) == 3 {
		return fmt.Sprintf("#%c%c%c%c%c%c", raw[0], raw[0], raw[1], raw[1], raw[2], raw[2])
	}
	// fallback: pad or trim to 6
	if len(raw) < 6 {
		raw = raw + strings.Repeat("0", 6-len(raw))
		return "#" + raw
	}
	return "#" + raw[:6]
}

// ...existing code...

func (fs *FileService) DeleteFile(fileID string) (File, error) {
	// Check if file exists
	var file File
	if err := fs.DB.Where("id = ?", fileID).First(&file).Error; err != nil {
		return file, err
	}

	// Soft delete: just mark is_delete = true
	if err := fs.DB.Model(&file).Update("is_delete", true).Error; err != nil {
		return file, err
	}

	return file, nil
}

func (fs *FileService) ResetFile(fileID string) (File, error) {
	var file File
	if err := fs.DB.Where("id = ?", fileID).First(&file).Error; err != nil {
		return file, err
	}

	// Reset soft delete: mark is_delete = false
	if err := fs.DB.Model(&file).Update("is_delete", false).Error; err != nil {
		return file, err
	}

	return file, nil
}

// parseCSVReader reads CSV file from multipart.File and returns headers + data rows
func parseCSVReader(file multipart.File) ([]string, [][]string, error) {
	defer file.Seek(0, 0) // reset file pointer if needed

	reader := csv.NewReader(file)
	allRows, err := reader.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read csv file: %w", err)
	}

	if len(allRows) < 1 {
		return nil, nil, fmt.Errorf("csv file is empty")
	}

	headers := allRows[0]
	dataRows := allRows[1:]

	return headers, dataRows, nil
}

func (fs *FileService) CreateAccess(input []FileAccess) error {
	if err := fs.DB.Create(&input).Error; err != nil {
		return err
	}
	return nil
}

func (fs *FileService) DeleteAccess(accessId string) error {
	// Check if access record exists
	var access FileAccess
	if err := fs.DB.Where("id = ?", accessId).First(&access).Error; err != nil {
		return err
	}

	// Delete access record
	if err := fs.DB.Delete(&access).Error; err != nil {
		return err
	}

	return nil
}

func (fs *FileService) GetFileAccess(fileId string) ([]FileAccessWithUser, error) {
	var results []FileAccessWithUser

	err := fs.DB.Table("file_access").
		Select("file_access.id, file_access.user_id, file_access.file_id, users.firstname, users.lastname").
		Joins("JOIN users ON users.id = file_access.user_id").
		Where("file_access.file_id = ?", fileId).
		Scan(&results).Error

	if err != nil {
		return nil, err
	}

	return results, nil
}

func (fs *FileService) GetFileHistory(fileId string) ([]FileVersionWithUser, error) {
	var file File
	if err := fs.DB.Where("id = ?", fileId).First(&file).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}

	selectColumns := []string{
		"file_version.id",
		"file_version.file_id",
		"file_version.filename",
		"users.firstname AS firstname",
		"users.lastname AS lastname",
		"file_version.created_at",
		"file_version.private",
		"file_version.is_delete",
		"file_version.size",
		"file_version.version",
		"file_version.rows",
		"file_version.columns_order",
	}
	queryColumns := append([]string{}, selectColumns...)
	if fs.DB.Migrator().HasTable(&FileVersion{}) && fs.DB.Migrator().HasColumn(&FileVersion{}, "description") {
		queryColumns = append(queryColumns, "file_version.description")
	} else {
		queryColumns = append(queryColumns, "'' AS description")
	}
	queryColumns = append(queryColumns, "file_version.inserted_by")
	if fs.DB.Migrator().HasTable(&FileVersion{}) && fs.DB.Migrator().HasColumn(&FileVersion{}, "reconciliation_status") {
		queryColumns = append(queryColumns, "file_version.reconciliation_status")
	} else {
		queryColumns = append(queryColumns, "'' AS reconciliation_status")
	}
	if fs.DB.Migrator().HasTable(&FileVersion{}) && fs.DB.Migrator().HasColumn(&FileVersion{}, "reconciliation_error") {
		queryColumns = append(queryColumns, "file_version.reconciliation_error")
	} else {
		queryColumns = append(queryColumns, "'' AS reconciliation_error")
	}
	if fs.DB.Migrator().HasTable(&FileVersion{}) && fs.DB.Migrator().HasColumn(&FileVersion{}, "reconciled_at") {
		queryColumns = append(queryColumns, "file_version.reconciled_at")
	} else {
		queryColumns = append(queryColumns, "NULL AS reconciled_at")
	}
	if fs.DB.Migrator().HasTable(&FileVersion{}) && fs.DB.Migrator().HasColumn(&FileVersion{}, "transition_operation") {
		queryColumns = append(queryColumns, "file_version.transition_operation")
	} else {
		queryColumns = append(queryColumns, "'' AS transition_operation")
	}

	var versionRows []fileVersionHistoryQueryRow
	query := fs.DB.Table("file_version").
		Select(strings.Join(queryColumns, ", ")).
		Joins("LEFT JOIN users ON users.id = file_version.inserted_by").
		Where("file_version.file_id = ?", file.ID)
	if fs.DB.Migrator().HasTable(&FileVersion{}) && fs.DB.Migrator().HasColumn(&FileVersion{}, "reconciliation_status") {
		query = query.Order(
			fmt.Sprintf(
				"file_version.version DESC, CASE file_version.reconciliation_status WHEN '%s' THEN 0 WHEN '%s' THEN 1 ELSE 2 END ASC, file_version.created_at DESC, file_version.id DESC",
				fileVersionStatusReady,
				fileVersionStatusProcessing,
			),
		)
	} else {
		query = query.Order("file_version.version DESC, file_version.created_at DESC, file_version.id DESC")
	}

	err := query.Scan(&versionRows).Error

	if err != nil {
		return nil, err
	}

	var aggregates []fileDataVersionHistoryAggregate
	if err := fs.DB.Table("file_data").
		Select("version, COUNT(*) AS rows, MIN(CAST(created_at AS TEXT)) AS created_at_raw, MAX(inserted_by) AS inserted_by").
		Where("file_id = ?", file.ID).
		Group("version").
		Order("version DESC").
		Scan(&aggregates).Error; err != nil {
		return nil, err
	}

	resultsByVersion := make(map[int]FileVersionWithUser)
	for _, row := range versionRows {
		if _, exists := resultsByVersion[row.Version]; exists {
			continue
		}
		entry := row.FileVersionWithUser
		resultsByVersion[row.Version] = entry
	}

	missingUserIDs := make(map[uint]struct{})
	for _, aggregate := range aggregates {
		if _, exists := resultsByVersion[aggregate.Version]; exists {
			continue
		}
		if aggregate.InsertedBy > 0 {
			missingUserIDs[aggregate.InsertedBy] = struct{}{}
		}
	}

	userNames := make(map[uint]historyUserName)
	if len(missingUserIDs) > 0 {
		userIDs := make([]uint, 0, len(missingUserIDs))
		for id := range missingUserIDs {
			userIDs = append(userIDs, id)
		}

		var names []historyUserName
		if err := fs.DB.Table("users").
			Select("id, firstname, lastname").
			Where("id IN ?", userIDs).
			Scan(&names).Error; err != nil {
			return nil, err
		}
		for _, name := range names {
			userNames[name.ID] = name
		}
	}

	for _, aggregate := range aggregates {
		if _, exists := resultsByVersion[aggregate.Version]; exists {
			continue
		}

		userName := userNames[aggregate.InsertedBy]
		columnsOrder := datatypes.JSON(nil)
		switch {
		case aggregate.Version == file.Version && len(bytes.TrimSpace(file.ColumnsOrder)) > 0:
			columnsOrder = file.ColumnsOrder
		case len(bytes.TrimSpace(file.ColumnsOrder)) > 0:
			columnsOrder = file.ColumnsOrder
		}

		reconciliationStatus := ""
		if aggregate.Version == file.Version {
			reconciliationStatus = fileVersionStatusReady
		}

		createdAt := parseHistoryTimestamp(aggregate.CreatedAtRaw)
		if createdAt.IsZero() {
			createdAt = file.CreatedAt
		}

		resultsByVersion[aggregate.Version] = FileVersionWithUser{
			FileID:               file.ID,
			FileName:             file.Filename,
			Firstname:            userName.Firstname,
			Lastname:             userName.Lastname,
			CreatedAt:            createdAt,
			Private:              file.Private,
			IsDelete:             file.IsDelete,
			Size:                 0,
			Version:              aggregate.Version,
			Rows:                 int(aggregate.Rows),
			ColumnsOrder:         columnsOrder,
			ReconciliationStatus: reconciliationStatus,
			ReconciliationError:  "",
			TransitionOperation:  "",
		}
	}

	if len(resultsByVersion) == 0 {
		return nil, nil
	}

	versions := make([]int, 0, len(resultsByVersion))
	for version := range resultsByVersion {
		versions = append(versions, version)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(versions)))

	results := make([]FileVersionWithUser, 0, len(versions))
	for _, version := range versions {
		entry := resultsByVersion[version]
		if entry.Rows <= 0 {
			for _, aggregate := range aggregates {
				if aggregate.Version == version {
					entry.Rows = int(aggregate.Rows)
					if entry.CreatedAt.IsZero() {
						entry.CreatedAt = parseHistoryTimestamp(aggregate.CreatedAtRaw)
					}
					break
				}
			}
		}
		results = append(results, entry)
	}

	return results, nil
}

func parseHistoryTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}

	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}

	return time.Time{}
}

func (fs *FileService) RevertFile(filename string, version int, userID uint) error {
	if err := ensureFileDescriptionSchema(fs.DB); err != nil {
		return err
	}

	var file File
	if err := fs.DB.Where("filename = ?", filename).First(&file).Error; err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	if err := fs.ensureNoOpenRequestsForFile(file, "revert"); err != nil {
		return err
	}
	if err := fs.ensureFileVersionTransitionIdle(file); err != nil {
		return err
	}

	return fs.DB.Transaction(func(tx *gorm.DB) error {
		lockedFile, err := lockFileForVersionTransition(tx, file.ID)
		if err != nil {
			return err
		}
		if err := ensureNoOpenRequestsForFileWithDB(tx, lockedFile, "revert"); err != nil {
			return err
		}
		if err := ensureFileVersionTransitionIdleWithDB(tx, lockedFile.ID); err != nil {
			var transitionErr *FileVersionTransitionError
			if errors.As(err, &transitionErr) {
				transitionErr.Filename = lockedFile.Filename
			}
			return err
		}

		var targetVersion FileVersion
		if err := preferredFileVersionQuery(tx, lockedFile.ID, version).First(&targetVersion).Error; err != nil {
			return fmt.Errorf("target version not found: %w", err)
		}

		var dataRows []FileData
		if err := tx.Where("file_id = ? AND version = ?", lockedFile.ID, version).Find(&dataRows).Error; err != nil {
			return err
		}

		newVersion, err := nextFileVersionNumber(tx, lockedFile.ID, lockedFile.Version)
		if err != nil {
			return err
		}

		newFileVersion := FileVersion{
			FileID:               lockedFile.ID,
			Filename:             lockedFile.Filename,
			Description:          targetVersion.Description,
			InsertedBy:           userID,
			CreatedAt:            time.Now(),
			Private:              targetVersion.Private,
			Version:              newVersion,
			IsDelete:             false,
			Rows:                 targetVersion.Rows,
			Size:                 targetVersion.Size,
			ColumnsOrder:         targetVersion.ColumnsOrder,
			ReconciliationStatus: fileVersionStatusProcessing,
			ReconciliationError:  "",
			TransitionOperation:  versionTransitionOperationRevert,
		}
		if err := tx.Create(&newFileVersion).Error; err != nil {
			return err
		}

		for _, row := range dataRows {
			newRow := FileData{
				FileID:     lockedFile.ID,
				RowData:    row.RowData,
				InsertedBy: userID,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
				Version:    newVersion,
			}
			if err := tx.Create(&newRow).Error; err != nil {
				return err
			}
		}

		if err := honour.ClearForFileTx(tx, lockedFile.ID); err != nil {
			return err
		}

		return fs.enqueueVersionTransitionJob(
			tx,
			lockedFile.ID,
			newFileVersion.ID,
			lockedFile.Version,
			newVersion,
			version,
			userID,
			versionTransitionOperationRevert,
		)
	})
}

func (fs *FileService) CreateEditRequest(input EditRequestInput, userID uint) (*FileEditRequest, error) {
	if err := EnsureFileVersionTransitionIdleByID(fs.DB, input.FileID); err != nil {
		return nil, err
	}

	// Step 1: Insert main request
	request := FileEditRequest{
		UserID:            userID,
		Status:            "pending",
		CreatedAt:         time.Now(),
		FirstName:         input.FirstName,
		LastName:          input.LastName,
		Consent:           input.Consent,
		ArchiveConsent:    input.ArchiveConsent,
		RowID:             input.RowID,
		IsEdited:          input.IsEdited,
		RequestType:       fileEditRequestTypeEdit,
		FileID:            input.FileID,
		Community:         input.Community,
		UploaderCommunity: input.UploaderCommunity,
	}

	// Keep as-is; if you ever see bool issues, use Select("*") here.
	if err := fs.DB.Create(&request).Error; err != nil {
		return nil, err
	}

	// Step 2: Insert change details
	var details []FileEditRequestDetails
	now := time.Now()
	rowId := input.RowID

	for _, edits := range input.Changes {
		for _, item := range edits {
			details = append(details, FileEditRequestDetails{
				RequestID: request.RequestID,
				FileID:    input.FileID,
				Filename:  input.Filename,
				RowID:     item.RowID,
				FieldName: item.FieldName,
				OldValue:  item.OldValue,
				NewValue:  item.NewValue,
				CreatedAt: now,
			})
		}
	}

	if len(details) > 0 {
		if err := fs.DB.Create(&details).Error; err != nil {
			return nil, err
		}
	}

	// Step 3: Upload images to GCS
	bucket := "nordik-drive-photos"
	timestamp := time.Now().Format("20060102150405")

	// ✅ Folder rule:
	// - if is_edited=false OR row_id==0 => requests/<requestID>_<first>_<last>
	// - else => requests/<row_id>
	var basePrefix string
	if input.IsEdited && rowId != 0 {
		basePrefix = util.RowPrefix(rowId)
	} else {
		basePrefix = util.TempPrefix(request.RequestID, input.FirstName, input.LastName)
	}

	// App photos
	// App photos
	for i, p := range input.PhotosInApp {
		ext := util.ExtFromFilenameOrMime(p.Filename, p.MimeType)
		fileName := fmt.Sprintf("%s_%s_%s_%d%s",
			input.FirstName,
			input.LastName,
			timestamp,
			i+1,
			ext,
		)

		objectPath := fmt.Sprintf("%s/%s", basePrefix, fileName)

		url, sizeBytes, err := uploadToGCSHook(p.DataBase64, bucket, objectPath)
		if err != nil {
			return nil, err
		}

		photoRecord := FileEditRequestPhoto{
			RequestID:        request.RequestID,
			RowID:            rowId,
			PhotoURL:         url,
			FileName:         fileName,
			SizeBytes:        sizeBytes,
			IsGalleryPhoto:   false,
			Status:           "pending",
			CreatedAt:        time.Now(),
			SourceFile:       input.Filename,
			FileID:           input.FileID,
			DocumentType:     "photos",
			DocumentCategory: "",
			PhotoComment:     util.ClampComment100(p.Comment), // NEW
		}

		if err := fs.DB.Create(&photoRecord).Error; err != nil {
			return nil, err
		}
	}

	// Gallery photos
	// Gallery photos
	for i, p := range input.PhotosForGallery {
		ext := util.ExtFromFilenameOrMime(p.Filename, p.MimeType)
		fileName := fmt.Sprintf("%s_%s_%s_gallery_%d%s",
			input.FirstName,
			input.LastName,
			timestamp,
			i+1,
			ext,
		)

		objectPath := fmt.Sprintf("%s/%s", basePrefix, fileName)

		url, sizeBytes, err := uploadToGCSHook(p.DataBase64, bucket, objectPath)
		if err != nil {
			return nil, err
		}

		photoRecord := FileEditRequestPhoto{
			RequestID:        request.RequestID,
			RowID:            rowId,
			PhotoURL:         url,
			FileName:         fileName,
			SizeBytes:        sizeBytes,
			IsGalleryPhoto:   true,
			Status:           "pending",
			CreatedAt:        time.Now(),
			SourceFile:       input.Filename,
			FileID:           input.FileID,
			DocumentType:     "photos",
			DocumentCategory: "",
			PhotoComment:     util.ClampComment100(p.Comment), // NEW
		}

		if err := fs.DB.Create(&photoRecord).Error; err != nil {
			return nil, err
		}
	}

	for i, doc := range input.Documents {
		// only accept document_type=document (ignore photos if any client sends)
		docType := doc.DocumentType
		if docType == "" {
			docType = "document"
		}
		if docType != "document" {
			continue
		}

		// name + path
		safeCategory := doc.DocumentCategory
		if safeCategory == "" {
			safeCategory = "other_document"
		}

		fileName := fmt.Sprintf("%s_%s_%s_doc_%d_%s",
			input.FirstName,
			input.LastName,
			timestamp,
			i+1,
			doc.Filename,
		)

		objectPath := fmt.Sprintf("%s/%s", basePrefix, fileName)

		// ✅ You can reuse UploadPhotoToGCS if it supports any base64 mime.
		// Better rename it to UploadBase64ToGCS.
		url, sizeBytes, err := uploadToGCSHook(doc.DataBase64, bucket, objectPath)
		if err != nil {
			return nil, err
		}

		rec := FileEditRequestPhoto{
			RequestID:      request.RequestID,
			RowID:          rowId,
			PhotoURL:       url,
			FileName:       fileName,
			SizeBytes:      sizeBytes,
			IsGalleryPhoto: false,
			Status:         "pending",
			CreatedAt:      time.Now(),
			SourceFile:     input.Filename,
			FileID:         input.FileID,

			DocumentType:     "document",
			DocumentCategory: safeCategory,
		}

		if err := fs.DB.Create(&rec).Error; err != nil {
			return nil, err
		}
	}

	return &request, nil
}

// CreateAchieverStoryRequest stores a proposed story as pending. It is linked
// to the existing request workflow so administrators can approve or reject it
// before the public row viewer exposes it.
func (fs *FileService) CreateAchieverStoryRequest(input AchieverStoryRequestInput, userID uint) (*FileEditRequest, error) {
	if err := EnsureFileVersionTransitionIdleByID(fs.DB, input.FileID); err != nil {
		return nil, err
	}

	storyType, contentType, err := validateAchieverStoryRequest(input)
	if err != nil {
		return nil, err
	}

	var request FileEditRequest
	if err := fs.DB.Transaction(func(tx *gorm.DB) error {
		var currentFile File
		if err := tx.Where("id = ?", input.FileID).First(&currentFile).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("file not found")
			}
			return err
		}

		var rowCount int64
		if err := tx.Model(&FileData{}).
			Where("id = ? AND file_id = ?", input.RowID, input.FileID).
			Count(&rowCount).Error; err != nil {
			return err
		}
		if rowCount == 0 {
			return fmt.Errorf("file row not found")
		}

		request = FileEditRequest{
			UserID:      userID,
			Status:      fileEditRequestStatusPending,
			RequestType: fileEditRequestTypeAchieverStory,
			FirstName:   strings.TrimSpace(input.FirstName),
			LastName:    strings.TrimSpace(input.LastName),
			RowID:       int(input.RowID),
			IsEdited:    true,
			FileID:      input.FileID,
		}
		if err := tx.Create(&request).Error; err != nil {
			return err
		}

		requestID := request.RequestID
		createdBy := userID
		storyText := strings.TrimSpace(input.StoryText)
		if input.Template != nil {
			storyText = strings.TrimSpace(input.Template.AchieversStory)
		}
		story := AchieverStory{
			FileID:      input.FileID,
			FileVersion: currentFile.Version,
			RowID:       input.RowID,
			RequestID:   &requestID,
			CreatedBy:   &createdBy,
			StoryType:   storyType,
			Status:      fileEditRequestStatusPending,
			StoryText:   storyText,
			VideoURL:    strings.TrimSpace(input.VideoURL),
			ContentType: contentType,
		}

		var storedFile *DocumentInput
		switch storyType {
		case "text":
			var pdfData string
			var err error
			if input.Template != nil {
				pdfData, err = buildAchieverStoryTemplatePDFDataURL(
					achieverStoryTemplatePDFTitle(input.FirstName, input.LastName),
					*input.Template,
				)
			} else {
				pdfData, err = buildAchieverStoryPDFDataURL(
					achieverStoryPDFTitle(input.FirstName, input.LastName),
					storyText,
				)
			}
			if err != nil {
				return fmt.Errorf("create story PDF: %w", err)
			}
			storedFile = &DocumentInput{
				Filename:   achieverStoryPDFFilename(input.FirstName, input.LastName),
				MimeType:   "application/pdf",
				DataBase64: pdfData,
			}
			story.ContentType = "application/pdf"
		case "video":
			storedFile = input.Video
		case "document":
			storedFile = input.Document
		}

		if storedFile != nil {
			fileName := storyStorageFilename(storedFile.Filename)
			objectPath := fmt.Sprintf(
				"achiever-stories/requests/%d/%s_%s",
				request.RequestID,
				time.Now().UTC().Format("20060102150405"),
				fileName,
			)
			storyURL, sizeBytes, err := uploadToGCSHook(storedFile.DataBase64, "nordik-drive-photos", objectPath)
			if err != nil {
				return fmt.Errorf("upload story file: %w", err)
			}
			story.StoryURL = storyURL
			story.FileName = fileName
			story.SizeBytes = sizeBytes
		}

		if err := tx.Create(&story).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &request, nil
}

func validateAchieverStoryRequest(input AchieverStoryRequestInput) (storyType string, contentType string, err error) {
	if input.FileID == 0 {
		return "", "", fmt.Errorf("file_id is required")
	}
	if input.RowID == 0 {
		return "", "", fmt.Errorf("row_id is required")
	}

	storyType = strings.ToLower(strings.TrimSpace(input.StoryType))
	switch storyType {
	case "text":
		if input.Template != nil {
			if strings.TrimSpace(input.Template.AchieversStory) == "" {
				return "", "", fmt.Errorf("template.achievers_story is required")
			}
			return storyType, "application/pdf", nil
		}
		if strings.TrimSpace(input.StoryText) == "" {
			return "", "", fmt.Errorf("story_text is required")
		}
		return storyType, "application/pdf", nil
	case "video":
		videoURL := strings.TrimSpace(input.VideoURL)
		if videoURL != "" && input.Video != nil {
			return "", "", fmt.Errorf("provide either video_url or video, not both")
		}
		if videoURL != "" {
			parsed, parseErr := url.ParseRequestURI(videoURL)
			if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" ||
				(parsed.Scheme != "https" && parsed.Scheme != "http") {
				return "", "", fmt.Errorf("video_url must be a valid http or https URL")
			}
			return storyType, "", nil
		}
		if input.Video == nil {
			return "", "", fmt.Errorf("video_url or video is required")
		}
		if strings.TrimSpace(input.Video.DataBase64) == "" {
			return "", "", fmt.Errorf("video data is required")
		}
		if input.Video.Size > 20*1024*1024 {
			return "", "", fmt.Errorf("uploaded video must be 20 MB or smaller")
		}

		name := strings.ToLower(strings.TrimSpace(input.Video.Filename))
		mime := strings.ToLower(strings.TrimSpace(input.Video.MimeType))
		switch {
		case strings.HasSuffix(name, ".mp4") || strings.HasSuffix(name, ".m4v") || mime == "video/mp4" || mime == "video/x-m4v":
			return storyType, "video/mp4", nil
		case strings.HasSuffix(name, ".webm") || mime == "video/webm":
			return storyType, "video/webm", nil
		case strings.HasSuffix(name, ".ogg") || strings.HasSuffix(name, ".ogv") || mime == "video/ogg":
			return storyType, "video/ogg", nil
		case strings.HasSuffix(name, ".mov") || mime == "video/quicktime":
			return storyType, "video/quicktime", nil
		default:
			return "", "", fmt.Errorf("uploaded video must be an MP4, WebM, OGG, or MOV file")
		}
	case "document":
		if input.Document == nil {
			return "", "", fmt.Errorf("document is required")
		}
		if strings.TrimSpace(input.Document.DataBase64) == "" {
			return "", "", fmt.Errorf("document data is required")
		}

		name := strings.ToLower(strings.TrimSpace(input.Document.Filename))
		mime := strings.ToLower(strings.TrimSpace(input.Document.MimeType))
		switch {
		case strings.HasSuffix(name, ".pdf") || mime == "application/pdf":
			return storyType, "application/pdf", nil
		case strings.HasSuffix(name, ".docx") || mime == "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
			return storyType, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", nil
		case strings.HasSuffix(name, ".doc") || mime == "application/msword":
			return storyType, "application/msword", nil
		default:
			return "", "", fmt.Errorf("story document must be a PDF, DOC, or DOCX file")
		}
	default:
		return "", "", fmt.Errorf("story_type must be text, video, or document")
	}
}

func storyStorageFilename(value string) string {
	name := filepath.Base(strings.TrimSpace(value))
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	if name == "" || name == "." {
		return "achiever_story"
	}
	return name
}

var validFileEditRequestStatuses = map[string]struct{}{
	"pending":   {},
	"completed": {},
}

func parseStatuses(csv string) ([]string, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}

	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}

	for _, p := range parts {
		s := strings.ToLower(strings.TrimSpace(p))
		if s == "" {
			continue
		}
		if _, ok := validFileEditRequestStatuses[s]; !ok {
			return nil, fmt.Errorf("status must be pending or completed")
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (fs *FileService) GetEditRequests(statusCSV *string, userID *uint) ([]FileEditRequestWithUser, error) {
	var baseRequests []struct {
		RequestID      uint
		RowID          int
		UserID         uint
		Firstname      string
		Lastname       string
		Status         string
		ReviewComment  string `gorm:"column:reviewer_comment"`
		CreatedAt      time.Time
		EFirstName     string `gorm:"column:efirstname"`
		ELastName      string `gorm:"column:elastname"`
		IsEdited       bool   `gorm:"default:true"`
		Consent        bool
		ArchiveConsent bool   `gorm:"column:archive_consent"`
		FileID         uint   `gorm:"column:file_id"`
		FileName       string `gorm:"column:file_name"`
		RequestType    string `gorm:"column:request_type"`
	}

	q := fs.DB.Table("file_edit_request").
		Select(`
			file_edit_request.request_id,
			file_edit_request.row_id,
			file_edit_request.user_id,
			users.firstname,
			users.lastname,
			file_edit_request.status,
			COALESCE(file_edit_request.reviewer_comment, '') as reviewer_comment,
			file_edit_request.created_at,
			file_edit_request.firstname as efirstname,
			file_edit_request.lastname as elastname,
			file_edit_request.is_edited,
			file_edit_request.consent,
			file_edit_request.archive_consent,
			file_edit_request.file_id,
			COALESCE(file.filename, '') AS file_name,
			COALESCE(file_edit_request.request_type, 'edit') AS request_type
		`).
		Joins("JOIN users ON users.id = file_edit_request.user_id").
		Joins("LEFT JOIN file ON file.id = file_edit_request.file_id").
		Order("file_edit_request.created_at DESC")

	var statuses []string
	if statusCSV != nil && strings.TrimSpace(*statusCSV) != "" {
		parsedStatuses, err := parseStatuses(*statusCSV)
		if err != nil {
			return nil, err
		}
		statuses = parsedStatuses
	}

	// ✅ Only if BOTH provided: filter by status IN (...) + user_id
	if len(statuses) > 0 && userID != nil && *userID > 0 {
		q = q.Where("file_edit_request.user_id = ?", *userID).
			Where("file_edit_request.status IN ?", statuses)
	} else {
		// ✅ Default behavior: pending only (exactly like today)
		q = q.Where("file_edit_request.status = ?", "pending")
	}

	if err := q.Scan(&baseRequests).Error; err != nil {
		return nil, err
	}

	requestIDs := make([]uint, 0, len(baseRequests))
	for _, req := range baseRequests {
		requestIDs = append(requestIDs, req.RequestID)
	}

	storiesByRequest := make(map[uint][]AchieverStory)
	if len(requestIDs) > 0 {
		var stories []AchieverStory
		if err := fs.DB.Where("request_id IN ?", requestIDs).
			Order("created_at ASC, id ASC").
			Find(&stories).Error; err != nil {
			return nil, err
		}
		for _, story := range stories {
			if story.RequestID != nil {
				storiesByRequest[*story.RequestID] = append(storiesByRequest[*story.RequestID], story)
			}
		}
	}

	final := make([]FileEditRequestWithUser, 0, len(baseRequests))

	for _, req := range baseRequests {
		var details []FileEditRequestDetails
		if err := fs.DB.Where("request_id = ?", req.RequestID).
			Order("id ASC").
			Find(&details).Error; err != nil {
			return nil, err
		}

		final = append(final, FileEditRequestWithUser{
			RequestID:      req.RequestID,
			UserID:         req.UserID,
			Firstname:      req.Firstname,
			Lastname:       req.Lastname,
			Status:         req.Status,
			RequestType:    req.RequestType,
			ReviewComment:  req.ReviewComment,
			CreatedAt:      req.CreatedAt,
			Details:        details,
			EFirstName:     req.EFirstName,
			ELastName:      req.ELastName,
			RowID:          req.RowID,
			IsEdited:       req.IsEdited,
			Consent:        req.Consent,
			ArchiveConsent: req.ArchiveConsent,
			FileID:         req.FileID,
			FileName:       req.FileName,
			Stories:        storiesByRequest[req.RequestID],
		})
	}

	return final, nil
}

var formSubmissionGoHook = func(fn func()) {
	go fn()
}

var triggerFormSubmissionReviewEmailHook = func(
	fileedit *FileEditRequest,
	user auth.Auth,
	details []FileEditRequestDetails,
	mailer mailer.EmailSender,
) error {

	createdUser := fmt.Sprintf("%s %s", user.FirstName, user.LastName)
	body := BuildFileEditRequestReviewEmailBody(
		createdUser,
		fileedit.Status,
		fileedit.FirstName,
		fileedit.LastName,
		fileedit.ReviewComment,
		details...,
	)

	err := mailer.SendOne(
		user.Email,
		"Update to your submission",
		body,
	)

	return err
}

func (fs *FileService) triggerReviewEmailAsync(fileedit *FileEditRequest) {
	formSubmissionGoHook(func() {
		defer func() {
			if recover() != nil {
				_ = fs.DB.Model(&FileEditRequest{}).
					Where("request_id = ?", fileedit.RequestID).
					Update("review_email_trigger_success", false).Error
			}
		}()

		var user auth.Auth
		if err := fs.DB.Where("id = ?", fileedit.UserID).First(&user).Error; err != nil {
			return
		}

		var details []FileEditRequestDetails
		if err := fs.DB.Where("request_id = ?", fileedit.RequestID).
			Order("id ASC").
			Find(&details).Error; err != nil {
			return
		}

		if err := triggerFormSubmissionReviewEmailHook(fileedit, user, details, fs.Mailer); err != nil {
			return
		}

		_ = fs.DB.Model(&FileEditRequest{}).
			Where("request_id = ?", fileedit.RequestID).
			Update("review_email_trigger_success", true).Error
	})
}

func (fs *FileService) ReviewEditRequest(
	requestID uint,
	status string,
	reviewComment string,
	updates []FileEditRequestDetails,
	userId uint,
) error {
	status = strings.ToLower(strings.TrimSpace(status))
	reviewComment = strings.TrimSpace(reviewComment)
	defaultDetailStatus := ReviewStatusApproved

	if requestID == 0 {
		return fmt.Errorf("request_id is required")
	}

	if status != "completed" {
		return fmt.Errorf("status must be completed")
	}

	var reviewedRequest *FileEditRequest

	if err := fs.DB.Transaction(func(tx *gorm.DB) error {
		hasDetailStatuses := false

		// 0) Update changed details coming from UI.
		for _, upd := range updates {
			if upd.ID == 0 {
				return fmt.Errorf("update id is required")
			}

			detailUpdates := map[string]interface{}{
				"new_value": upd.NewValue,
			}

			detailStatus := strings.ToLower(strings.TrimSpace(upd.Status))
			if detailStatus != "" {
				if detailStatus != "pending" && detailStatus != "approved" && detailStatus != "rejected" {
					return fmt.Errorf("detail status must be pending, approved, or rejected")
				}
				hasDetailStatuses = true
				detailUpdates["status"] = detailStatus
				detailUpdates["reviewer_comment"] = strings.TrimSpace(upd.ReviewComment)
			}

			if err := tx.Model(&FileEditRequestDetails{}).
				Where("id = ? AND request_id = ?", upd.ID, requestID).
				Updates(detailUpdates).Error; err != nil {
				return err
			}
		}

		// 1) Load request
		var req FileEditRequest
		if err := tx.Where("request_id = ?", requestID).First(&req).Error; err != nil {
			return err
		}

		if req.RequestType == fileEditRequestTypeAchieverStory {
			return fmt.Errorf("achiever story requests must be reviewed with story decisions")
		}

		currentFile := File{ID: req.FileID, Version: 1}
		if err := tx.Where("id = ?", req.FileID).First(&currentFile).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		if !hasDetailStatuses {
			if err := tx.Model(&FileEditRequestDetails{}).
				Where("request_id = ?", requestID).
				Updates(map[string]interface{}{
					"status":           defaultDetailStatus,
					"reviewer_comment": reviewComment,
				}).Error; err != nil {
				return err
			}
		}

		// 2) Fetch all details for this request after review fields are applied.
		var allDetails []FileEditRequestDetails
		if err := tx.Where("request_id = ?", requestID).Find(&allDetails).Error; err != nil {
			return err
		}

		approvedDetails := make([]FileEditRequestDetails, 0, len(allDetails))
		for _, detail := range allDetails {
			if strings.ToLower(strings.TrimSpace(detail.Status)) == ReviewStatusApproved {
				approvedDetails = append(approvedDetails, detail)
			}
		}

		// Only approved detail rows are applied to file_data.
		if len(approvedDetails) > 0 {
			// 3) If is_edited=false => create new FileData row FIRST, use its ID as rowID
			var finalRowID uint
			if !req.IsEdited {
				// Build row_data JSON from details (field -> new_value)
				row := make(map[string]string, len(approvedDetails))
				for _, d := range approvedDetails {
					row[d.FieldName] = d.NewValue
				}

				newJSON, err := json.Marshal(row)
				if err != nil {
					return fmt.Errorf("failed to build row_data json: %v", err)
				}

				// Create file_data row (row_id will be new FileData.ID)
				fileID := approvedDetails[0].FileID

				fd := FileData{
					FileID:     fileID,
					RowData:    datatypes.JSON(newJSON),
					InsertedBy: req.UserID,
					CreatedAt:  time.Now(),
					UpdatedAt:  time.Now(),
					Version:    currentFile.Version,
				}

				if err := tx.Create(&fd).Error; err != nil {
					return fmt.Errorf("failed to insert file_data: %v", err)
				}

				finalRowID = fd.ID

				// Update all details row_id
				if err := tx.Model(&FileEditRequestDetails{}).
					Where("request_id = ?", requestID).
					Update("row_id", finalRowID).Error; err != nil {
					return err
				}

				// Update request row_id
				if err := tx.Model(&FileEditRequest{}).
					Where("request_id = ?", requestID).
					Update("row_id", finalRowID).Error; err != nil {
					return err
				}

				// Move photos folder: requests/<requestID>_<first>_<last> -> requests/<rowID>
				bucket := "nordik-drive-photos"

				srcPrefix := util.TempPrefix(req.RequestID, req.FirstName, req.LastName)
				dstPrefix := util.RowPrefix(int(finalRowID))

				mapping, err := moveGCSFolderHook(bucket, srcPrefix, dstPrefix)
				if err != nil {
					return err
				}

				// Update photo rows: row_id + photo_url
				var photos []FileEditRequestPhoto
				if err := tx.Where("request_id = ?", requestID).Find(&photos).Error; err != nil {
					return err
				}

				for _, p := range photos {
					oldObj := fmt.Sprintf("%s/%s", srcPrefix, p.FileName)

					newObj, ok := mapping[oldObj]
					if !ok {
						newObj = fmt.Sprintf("%s/%s", dstPrefix, p.FileName)
					}

					newURL := fmt.Sprintf("gs://%s/%s", bucket, newObj)

					if err := tx.Model(&FileEditRequestPhoto{}).
						Where("id = ?", p.ID).
						Updates(map[string]any{
							"row_id":    finalRowID,
							"photo_url": newURL,
						}).Error; err != nil {
						return err
					}
				}
			} else {
				// 4) Existing edit: update the existing file_data row
				for _, det := range approvedDetails {
					var fileData FileData
					err := tx.Where("file_id = ? AND id = ?", det.FileID, det.RowID).First(&fileData).Error
					if err != nil {
						return fmt.Errorf("file data row not found for file %d row %d", det.FileID, det.RowID)
					}

					var row map[string]string
					if err := json.Unmarshal(fileData.RowData, &row); err != nil {
						return fmt.Errorf("failed to parse row_data: %v", err)
					}

					row[det.FieldName] = det.NewValue

					newJSON, err := json.Marshal(row)
					if err != nil {
						return fmt.Errorf("failed to build updated row_data json: %v", err)
					}

					if err := tx.Model(&fileData).
						Updates(map[string]interface{}{
							"row_data":   datatypes.JSON(newJSON),
							"updated_at": time.Now(),
						}).Error; err != nil {
						return fmt.Errorf("failed to update file_data: %v", err)
					}
				}
			}

			approvedFields := make([]string, 0, len(approvedDetails))
			for _, detail := range approvedDetails {
				approvedFields = append(approvedFields, detail.FieldName)
			}
			if err := syncCurrentVersionMetadata(tx, currentFile, approvedFields); err != nil {
				return err
			}
		}

		// 5) Save request review result
		reviewedByID := int(userId)

		if err := tx.Model(&FileEditRequest{}).
			Where("request_id = ?", requestID).
			Updates(map[string]interface{}{
				"status":           status,
				"reviewed_by":      reviewedByID,
				"reviewer_comment": reviewComment,
			}).Error; err != nil {
			return err
		}

		req.Status = status
		req.ReviewComment = reviewComment
		req.ReviewedBy = &reviewedByID
		reviewedRequest = &req

		return nil
	}); err != nil {
		return err
	}

	fs.triggerReviewEmailAsync(reviewedRequest)

	return nil
}

// ReviewAchieverStoryRequest completes an achiever-story request only after
// every submitted story has a decision. Approved stories become visible in the
// row's public story viewer; rejected stories remain private to the request.
func (fs *FileService) ReviewAchieverStoryRequest(
	requestID uint,
	reviewComment string,
	storyReviews []AchieverStoryReviewInput,
	userID uint,
) error {
	if requestID == 0 {
		return fmt.Errorf("request_id is required")
	}

	var reviewedRequest *FileEditRequest
	if err := fs.DB.Transaction(func(tx *gorm.DB) error {
		var req FileEditRequest
		if err := tx.Where("request_id = ?", requestID).First(&req).Error; err != nil {
			return err
		}
		if req.RequestType != fileEditRequestTypeAchieverStory {
			return fmt.Errorf("request is not an achiever story request")
		}
		if req.Status != fileEditRequestStatusPending {
			return fmt.Errorf("request has already been reviewed")
		}

		if err := reviewAchieverStoryRequest(tx, requestID, storyReviews, userID); err != nil {
			return err
		}

		reviewedByID := int(userID)
		reviewComment = strings.TrimSpace(reviewComment)
		if err := tx.Model(&FileEditRequest{}).
			Where("request_id = ?", requestID).
			Updates(map[string]interface{}{
				"status":           fileEditRequestStatusCompleted,
				"reviewed_by":      reviewedByID,
				"reviewer_comment": reviewComment,
			}).Error; err != nil {
			return err
		}

		req.Status = fileEditRequestStatusCompleted
		req.ReviewComment = reviewComment
		req.ReviewedBy = &reviewedByID
		reviewedRequest = &req
		return nil
	}); err != nil {
		return err
	}

	fs.triggerReviewEmailAsync(reviewedRequest)
	return nil
}

func reviewAchieverStoryRequest(tx *gorm.DB, requestID uint, reviews []AchieverStoryReviewInput, reviewerID uint) error {
	var stories []AchieverStory
	if err := tx.Where("request_id = ?", requestID).Order("id ASC").Find(&stories).Error; err != nil {
		return err
	}
	if len(stories) == 0 {
		return fmt.Errorf("achiever story request has no stories")
	}
	if len(reviews) == 0 {
		return fmt.Errorf("approve or reject every achiever story before completing the request")
	}

	storyIDs := make(map[uint]struct{}, len(stories))
	for _, story := range stories {
		storyIDs[story.ID] = struct{}{}
	}

	now := time.Now()
	reviewerIDPtr := uint(reviewerID)
	for _, review := range reviews {
		if review.StoryID == 0 {
			return fmt.Errorf("story_id is required")
		}
		if _, ok := storyIDs[review.StoryID]; !ok {
			return fmt.Errorf("story %d does not belong to request", review.StoryID)
		}

		decision := strings.ToLower(strings.TrimSpace(review.Status))
		if decision != ReviewStatusApproved && decision != ReviewStatusRejected {
			return fmt.Errorf("story status must be approved or rejected")
		}
		comment := strings.TrimSpace(review.ReviewComment)
		if decision == ReviewStatusRejected && comment == "" {
			return fmt.Errorf("review comment is required for rejected stories")
		}

		if err := tx.Model(&AchieverStory{}).
			Where("id = ? AND request_id = ?", review.StoryID, requestID).
			Updates(map[string]interface{}{
				"status":           decision,
				"reviewed_by":      reviewerIDPtr,
				"reviewer_comment": comment,
				"reviewed_at":      now,
			}).Error; err != nil {
			return err
		}
	}

	var pendingCount int64
	if err := tx.Model(&AchieverStory{}).
		Where("request_id = ? AND status = ?", requestID, fileEditRequestStatusPending).
		Count(&pendingCount).Error; err != nil {
		return err
	}
	if pendingCount > 0 {
		return fmt.Errorf("approve or reject every achiever story before completing the request")
	}

	return nil
}

func (fs *FileService) GetPhotosByRequest(requestID uint) ([]FileEditRequestPhoto, error) {
	var photos []FileEditRequestPhoto

	if err := fs.DB.Where("request_id = ? AND document_type = ?", requestID, "photos").Find(&photos).Error; err != nil {
		return nil, err
	}

	return photos, nil
}

func (fs *FileService) GetDocsByRequest(requestID uint) ([]FileEditRequestPhoto, error) {
	var documents []FileEditRequestPhoto

	if err := fs.DB.Where("request_id = ? AND document_type = ?", requestID, "document").Find(&documents).Error; err != nil {
		return nil, err
	}
	return documents, nil
}

func (fs *FileService) GetPhotosByRow(requestID uint) ([]FileEditRequestPhoto, error) {
	var photos []FileEditRequestPhoto

	if err := fs.DB.Where("row_id = ? and status = ? and document_type = ?", requestID, "approved", "photos").Find(&photos).Error; err != nil {
		return nil, err
	}

	return photos, nil
}

func (fs *FileService) GetDocsByRow(requestID uint) ([]FileEditRequestPhoto, error) {
	var documents []FileEditRequestPhoto

	if err := fs.DB.Where("row_id = ? and status = ? and document_type = ?", requestID, "approved", "document").Find(&documents).Error; err != nil {
		return nil, err
	}
	return documents, nil
}

func (fs *FileService) GetAchieverStoriesByRow(rowID uint) ([]AchieverStory, error) {
	var stories []AchieverStory
	if err := fs.DB.
		Where("row_id = ? AND status = ?", rowID, "approved").
		Order("created_at ASC, id ASC").
		Find(&stories).Error; err != nil {
		return nil, err
	}
	return stories, nil
}

func (fs *FileService) GetPhotoBytes(photoID uint) ([]byte, string, error) {
	var photo FileEditRequestPhoto

	// 1. Fetch photo record
	if err := fs.DB.First(&photo, photoID).Error; err != nil {
		return nil, "", err
	}

	bucketName := os.Getenv("BUCKET_NAME")

	// 2. Extract object path from: gs://bucket/OBJECT_PATH
	prefix := "gs://" + bucketName + "/"
	objectPath := strings.TrimPrefix(photo.PhotoURL, prefix)

	ctx := context.Background()
	client, err := newGCSClientHook(ctx)
	if err != nil {
		return nil, "", err
	}
	defer client.Close()

	// 3. Read the file
	rc, err := client.Bucket(bucketName).Object(objectPath).NewReader(ctx)
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", err
	}

	contentType := rc.ContentType()

	return data, contentType, nil
}

func (fs *FileService) ReviewPhotos(reviews []PhotoReviewInput, reviewerID uint) error {
	if len(reviews) == 0 {
		return fmt.Errorf("at least one photo review is required")
	}

	now := time.Now()
	reviewerIDInt := int(reviewerID)

	return fs.DB.Transaction(func(tx *gorm.DB) error {
		for _, review := range reviews {
			if review.PhotoID == 0 {
				return fmt.Errorf("photo_id is required")
			}

			status := strings.ToLower(strings.TrimSpace(review.Status))
			if status != "approved" && status != "rejected" {
				return fmt.Errorf("status must be either approved or rejected")
			}

			if err := tx.Model(&FileEditRequestPhoto{}).
				Where("id = ?", review.PhotoID).
				Updates(map[string]interface{}{
					"status":           status,
					"reviewed_by":      reviewerIDInt,
					"reviewer_comment": strings.TrimSpace(review.ReviewComment),
					"reviewed_at":      now,
				}).Error; err != nil {
				return fmt.Errorf("failed to update photo %d: %v", review.PhotoID, err)
			}
		}

		return nil
	})
}

func (fs *FileService) GetDocBytes(docID uint) ([]byte, string, string, error) {
	var doc FileEditRequestPhoto

	// 1) Fetch record (docs are stored in file_edit_request_photos)
	if err := fs.DB.First(&doc, docID).Error; err != nil {
		return nil, "", "", err
	}

	// Optional safety: ensure it's a "document"
	// (remove if you want same endpoint to serve both)
	if doc.DocumentType != "document" {
		return nil, "", "", fmt.Errorf("requested item is not a document")
	}

	bucketName := os.Getenv("BUCKET_NAME")
	if bucketName == "" {
		return nil, "", "", fmt.Errorf("BUCKET_NAME env not set")
	}

	// 2) Extract object path from gs://bucket/OBJECT_PATH
	prefix := "gs://" + bucketName + "/"
	objectPath := strings.TrimPrefix(doc.PhotoURL, prefix)

	ctx := context.Background()
	client, err := newGCSClientHook(ctx)
	if err != nil {
		return nil, "", "", err
	}
	defer client.Close()

	// 3) Read file from GCS
	rc, err := client.Bucket(bucketName).Object(objectPath).NewReader(ctx)
	if err != nil {
		return nil, "", "", err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", "", err
	}

	contentType := rc.ContentType()
	if contentType == "" {
		// fallback (optional) if GCS contentType missing
		contentType = http.DetectContentType(data)
	}

	// Prefer DB filename if present
	filename := doc.FileName
	if filename == "" {
		filename = path.Base(objectPath)
	}

	return data, contentType, filename, nil
}

func (h *gcsReadHandle) Close() error {
	if h.Reader != nil {
		_ = h.Reader.Close()
	}
	if h.Client != nil {
		_ = h.Client.Close()
	}
	return nil
}

// OpenMediaHandle opens a streaming reader for a FileEditRequestPhoto row by its ID.
// kind is optional; if provided it enforces type ("photo" => DocumentType must be "photos",
// "doc"/"document" => DocumentType must be "document")
func (fs *FileService) OpenMediaHandle(ctx context.Context, id uint, kind string) (io.ReadCloser, string, string, string, error) {
	var rec FileEditRequestPhoto
	if err := fs.DB.First(&rec, id).Error; err != nil {
		return nil, "", "", "", err
	}

	// Optional guard on kind
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "" {
		if kind == "photo" && rec.DocumentType != "photos" {
			return nil, "", "", "", fmt.Errorf("requested item is not a photo")
		}
		if (kind == "doc" || kind == "document") && rec.DocumentType != "document" {
			return nil, "", "", "", fmt.Errorf("requested item is not a document")
		}
	}

	// Parse gs://bucket/object
	bucketFromURL, objectPath, err := parseGSURL(rec.PhotoURL)
	if err != nil {
		return nil, "", "", "", err
	}

	// If BUCKET_NAME is set, prefer it ONLY if URL bucket is empty (normally URL bucket exists)
	bucketName := bucketFromURL
	if bucketName == "" {
		bucketName = os.Getenv("BUCKET_NAME")
	}
	if bucketName == "" {
		return nil, "", "", "", fmt.Errorf("bucket name not found (gs url + BUCKET_NAME empty)")
	}

	client, err := newGCSClientHook(ctx)
	if err != nil {
		return nil, "", "", "", err
	}

	reader, err := client.Bucket(bucketName).Object(objectPath).NewReader(ctx)
	if err != nil {
		_ = client.Close()
		return nil, "", "", "", err
	}

	contentType := reader.ContentType()
	if contentType == "" {
		// controller will sniff if needed
		contentType = ""
	}

	filename := strings.TrimSpace(rec.FileName)
	if filename == "" {
		filename = path.Base(objectPath)
		if filename == "" {
			filename = fmt.Sprintf("file_%d", rec.ID)
		}
	}

	disposition := "attachment"
	if strings.HasPrefix(contentType, "image/") || contentType == "application/pdf" {
		disposition = "inline"
	}

	// If contentType is still empty, controller will sniff and recompute disposition

	return &gcsReadHandle{
		Client: client,
		Reader: reader,
	}, filename, contentType, disposition, nil

}

// OpenAchieverStoryHandle opens the stored document for an approved achiever
// story without loading it fully into server memory.
func (fs *FileService) OpenAchieverStoryHandle(ctx context.Context, id uint) (io.ReadCloser, string, string, string, error) {
	var story AchieverStory
	if err := fs.DB.Where("id = ? AND status = ?", id, "approved").First(&story).Error; err != nil {
		return nil, "", "", "", err
	}
	return fs.openAchieverStoryDocument(ctx, story)
}

// OpenRequestedAchieverStoryHandle lets the submitter or an administrator
// preview a pending document during the request workflow. Other users cannot
// use this route to access an unapproved story.
func (fs *FileService) OpenRequestedAchieverStoryHandle(ctx context.Context, id uint, userID uint, isAdmin bool) (io.ReadCloser, string, string, string, error) {
	query := fs.DB.Table((AchieverStory{}).TableName()+" s").
		Select("s.*").
		Joins("JOIN file_edit_request r ON r.request_id = s.request_id").
		Where("s.id = ?", id)
	if !isAdmin {
		query = query.Where("r.user_id = ?", userID)
	}

	var story AchieverStory
	if err := query.First(&story).Error; err != nil {
		return nil, "", "", "", err
	}
	return fs.openAchieverStoryDocument(ctx, story)
}

func (fs *FileService) openAchieverStoryDocument(ctx context.Context, story AchieverStory) (io.ReadCloser, string, string, string, error) {
	if strings.TrimSpace(story.StoryURL) == "" {
		return nil, "", "", "", fmt.Errorf("story has no stored file")
	}

	bucketName, objectPath, err := parseGSURL(story.StoryURL)
	if err != nil {
		return nil, "", "", "", err
	}

	client, err := newGCSClientHook(ctx)
	if err != nil {
		return nil, "", "", "", err
	}
	reader, err := client.Bucket(bucketName).Object(objectPath).NewReader(ctx)
	if err != nil {
		_ = client.Close()
		return nil, "", "", "", err
	}

	contentType := strings.TrimSpace(story.ContentType)
	if contentType == "" {
		contentType = reader.ContentType()
	}
	filename := strings.TrimSpace(story.FileName)
	if filename == "" {
		filename = path.Base(objectPath)
	}
	if filename == "" {
		filename = fmt.Sprintf("achiever_story_%d", story.ID)
	}

	disposition := "attachment"
	if strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "video/") || contentType == "application/pdf" {
		disposition = "inline"
	}

	return &gcsReadHandle{Client: client, Reader: reader}, filename, contentType, disposition, nil
}

// ✅ Missing helper you asked for: readFromGCS
// Use this when you want full bytes (not recommended for huge files).
func (fs *FileService) readFromGCS(gsURL string, dbFilename string) ([]byte, string, string, error) {
	bucket, objectPath, err := parseGSURL(gsURL)
	if err != nil {
		return nil, "", "", err
	}
	if bucket == "" {
		bucket = os.Getenv("BUCKET_NAME")
	}
	if bucket == "" {
		return nil, "", "", fmt.Errorf("bucket name not found (gs url + BUCKET_NAME empty)")
	}

	ctx := context.Background()
	client, err := newGCSClientHook(ctx)
	if err != nil {
		return nil, "", "", err
	}
	defer client.Close()

	rc, err := client.Bucket(bucket).Object(objectPath).NewReader(ctx)
	if err != nil {
		return nil, "", "", err
	}
	defer rc.Close()

	data, err := ioReadAll(rc)
	if err != nil {
		return nil, "", "", err
	}

	contentType := rc.ContentType()
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}

	filename := strings.TrimSpace(dbFilename)
	if filename == "" {
		filename = path.Base(objectPath)
		if filename == "" {
			filename = "file"
		}
	}

	return data, contentType, filename, nil
}

// parseGSURL parses gs://bucket/object
func parseGSURL(gsURL string) (bucket string, objectPath string, err error) {
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

// small wrapper so you don't need to import io everywhere in this file
func ioReadAll(r *storage.Reader) ([]byte, error) {
	// storage.Reader implements io.Reader
	// keep it simple:
	buf := new(strings.Builder)
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			if err == context.Canceled {
				return nil, err
			}
			if err == context.DeadlineExceeded {
				return nil, err
			}
			// many readers return io.EOF, but storage may wrap; we’ll treat any error containing EOF as EOF
			if strings.Contains(strings.ToLower(err.Error()), "eof") {
				break
			}
			return nil, err
		}
	}
	return []byte(buf.String()), nil
}

func (h *gcsReadHandle) Read(p []byte) (int, error) {
	return h.Reader.Read(p)
}

func ensureFileDescriptionSchema(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db not initialized")
	}
	if !db.Migrator().HasTable(&File{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&File{}, "Description") {
		if err := db.Migrator().AddColumn(&File{}, "Description"); err != nil {
			return err
		}
	}
	if db.Migrator().HasTable(&FileVersion{}) && !db.Migrator().HasColumn(&FileVersion{}, "Description") {
		if err := db.Migrator().AddColumn(&FileVersion{}, "Description"); err != nil {
			return err
		}
	}
	return nil
}

func normalizeUploadDescription(descriptions []string, index int) string {
	if index < 0 || index >= len(descriptions) {
		return ""
	}
	return strings.TrimSpace(descriptions[index])
}
