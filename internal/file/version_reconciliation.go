package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nordik-drive-api/internal/dataconfig"
	"nordik-drive-api/internal/requestscope"
)

const (
	fileVersionStatusReady      = "ready"
	fileVersionStatusProcessing = "processing"
	fileVersionStatusFailed     = "failed"

	versionTransitionOperationReplace = "replace"
	versionTransitionOperationRevert  = "revert"

	reconciliationJobStatusPending    = "pending"
	reconciliationJobStatusProcessing = "processing"
	reconciliationJobStatusCompleted  = "completed"
	reconciliationJobStatusFailed     = "failed"

	defaultVersionReconciliationMaxJobs     = 5
	defaultVersionReconciliationThreshold   = 45.0
	defaultVersionReconciliationMargin      = 8.0
	defaultVersionReconciliationHeartbeat   = 30 * time.Second
	defaultVersionReconciliationLeaseTTL    = 15 * time.Minute
	defaultVersionReconciliationRelinkBatch = 250
	maxVersionReconciliationAttempts        = 10
	maxGenericFieldScore                    = 12.0
	maxGenericFieldWeight                   = 3.0
	defaultVersionReconciliationRetryDelay  = time.Minute
)

var ErrFileVersionTransitionInProgress = errors.New("file version transition already in progress")
var ErrFileVersionTargetVersionConflict = errors.New("file version target version conflict")

type FileVersionTransitionError struct {
	FileID   uint
	Filename string
	State    string
}

func (e *FileVersionTransitionError) Error() string {
	state := strings.TrimSpace(e.State)
	if state == "" {
		state = "processing"
	}

	filename := strings.TrimSpace(e.Filename)
	switch {
	case filename != "":
		return fmt.Sprintf("file %q already has a version transition in %s state", filename, state)
	case e.FileID > 0:
		return fmt.Sprintf("file %d already has a version transition in %s state", e.FileID, state)
	default:
		return fmt.Sprintf("file already has a version transition in %s state", state)
	}
}

func (e *FileVersionTransitionError) Is(target error) bool {
	return target == ErrFileVersionTransitionInProgress
}

type FileVersionTargetVersionConflictError struct {
	FileID         uint
	FileVersionID  uint
	TargetVersion  int
	ConflictingIDs []uint
}

func (e *FileVersionTargetVersionConflictError) Error() string {
	if e == nil {
		return "file version target version conflict"
	}
	return fmt.Sprintf(
		"file %d target version %d conflicts with existing version rows %v",
		e.FileID,
		e.TargetVersion,
		e.ConflictingIDs,
	)
}

func (e *FileVersionTargetVersionConflictError) Is(target error) bool {
	return target == ErrFileVersionTargetVersionConflict
}

type FileVersionReconciliationJob struct {
	ID                  uint           `gorm:"primaryKey" json:"id"`
	FileID              uint           `gorm:"not null;index:idx_file_version_recon_jobs_file_status,priority:1" json:"file_id"`
	FileVersionID       uint           `gorm:"not null;uniqueIndex" json:"file_version_id"`
	SourceVersion       int            `gorm:"not null" json:"source_version"`
	TargetVersion       int            `gorm:"not null" json:"target_version"`
	MaterializedVersion int            `gorm:"not null;default:0" json:"materialized_version"`
	RequestedBy         uint           `gorm:"not null" json:"requested_by"`
	Operation           string         `gorm:"type:varchar(20);not null" json:"operation"`
	Status              string         `gorm:"type:varchar(20);not null;index:idx_file_version_recon_jobs_file_status,priority:2" json:"status"`
	Attempts            int            `gorm:"not null;default:0" json:"attempts"`
	LastError           string         `gorm:"type:text;not null;default:''" json:"last_error"`
	Payload             datatypes.JSON `gorm:"type:jsonb" json:"payload"`
	AvailableAt         time.Time      `gorm:"not null;index" json:"available_at"`
	StartedAt           *time.Time     `json:"started_at"`
	CompletedAt         *time.Time     `json:"completed_at"`
	CreatedAt           time.Time      `gorm:"not null;autoCreateTime" json:"created_at"`
	UpdatedAt           time.Time      `gorm:"not null;autoUpdateTime" json:"updated_at"`
}

func (FileVersionReconciliationJob) TableName() string {
	return "file_version_reconciliation_jobs"
}

type FileRowLineage struct {
	ID            uint           `gorm:"primaryKey" json:"id"`
	FileID        uint           `gorm:"not null;index:idx_file_row_lineage_target,priority:1" json:"file_id"`
	TargetVersion int            `gorm:"not null;uniqueIndex:uq_file_row_lineage_target_source,priority:1;index:idx_file_row_lineage_target,priority:2" json:"target_version"`
	SourceVersion int            `gorm:"not null" json:"source_version"`
	SourceRowID   uint           `gorm:"not null;uniqueIndex:uq_file_row_lineage_target_source,priority:2" json:"source_row_id"`
	TargetRowID   *uint          `gorm:"index" json:"target_row_id"`
	Status        string         `gorm:"type:varchar(20);not null" json:"status"`
	Method        string         `gorm:"type:varchar(50);not null" json:"method"`
	Score         float64        `gorm:"not null;default:0" json:"score"`
	DebugJSON     datatypes.JSON `gorm:"type:jsonb" json:"debug_json"`
	CreatedAt     time.Time      `gorm:"not null;autoCreateTime" json:"created_at"`
	UpdatedAt     time.Time      `gorm:"not null;autoUpdateTime" json:"updated_at"`
}

func (FileRowLineage) TableName() string {
	return "file_row_lineage"
}

type VersionReconciliationRunOptions struct {
	MaxJobs int
}

type VersionReconciliationRunResult struct {
	Claimed   int `json:"claimed"`
	Completed int `json:"completed"`
	Retried   int `json:"retried"`
	Failed    int `json:"failed"`
}

type versionReconciliationConfig struct {
	FieldHeaders map[string]string
	Weights      map[string]float64
	Threshold    float64
	Margin       float64
}

type storedVersionRow struct {
	ID         uint           `gorm:"column:id"`
	FileID     uint           `gorm:"column:file_id"`
	RowData    datatypes.JSON `gorm:"column:row_data"`
	InsertedBy uint           `gorm:"column:inserted_by"`
	CreatedAt  time.Time      `gorm:"column:created_at"`
	UpdatedAt  time.Time      `gorm:"column:updated_at"`
	Version    int            `gorm:"column:version"`
}

type versionRowDescriptor struct {
	storedVersionRow
	RowDataMap      map[string]string
	Features        rowMatchFeatures
	ApprovedUpdates map[string]string
	HasLinkedState  bool
}

type rowMatchFeatures struct {
	LogicalFields map[string]string
	RawFields     map[string]string
	Dates         map[string]*normalizedDate
	BlockKeys     []string
}

type exactMatchTier struct {
	Name                string
	Fields              []string
	RequiredEmptyFields []string
}

type rowMatchProposal struct {
	SourceID uint
	TargetID uint
	Method   string
	Score    float64
}

type rowLinkPlan struct {
	SourceID uint
	TargetID uint
	Method   string
	Score    float64
}

type sourceApprovedDetailRow struct {
	RowID     int       `gorm:"column:row_id"`
	FieldName string    `gorm:"column:field_name"`
	NewValue  string    `gorm:"column:new_value"`
	RequestID uint      `gorm:"column:request_id"`
	CreatedAt time.Time `gorm:"column:created_at"`
	DetailID  uint      `gorm:"column:detail_id"`
	IsEdited  bool      `gorm:"column:is_edited"`
}

type debugLineagePayload struct {
	Method        string   `json:"method"`
	SourceVersion int      `json:"source_version"`
	TargetVersion int      `json:"target_version"`
	Score         float64  `json:"score"`
	Matched       bool     `json:"matched"`
	TargetRowID   *uint    `json:"target_row_id,omitempty"`
	Fields        []string `json:"fields,omitempty"`
}

type versionReconciliationHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func EnsureFileVersionTransitionIdleByID(db *gorm.DB, fileID uint) error {
	if fileID == 0 {
		return nil
	}
	if db == nil {
		return fmt.Errorf("db not initialized")
	}
	if !db.Migrator().HasTable(&FileVersion{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&FileVersion{}, "reconciliation_status") {
		return nil
	}
	return ensureFileVersionTransitionIdleWithDB(db, fileID)
}

func preferredFileVersionQuery(db *gorm.DB, fileID uint, version int) *gorm.DB {
	query := db.Where("file_id = ? AND version = ?", fileID, version)
	if db != nil && db.Migrator().HasTable(&FileVersion{}) && db.Migrator().HasColumn(&FileVersion{}, "reconciliation_status") {
		query = query.Order(
			fmt.Sprintf(
				"CASE reconciliation_status WHEN '%s' THEN 0 WHEN '%s' THEN 1 ELSE 2 END ASC",
				fileVersionStatusReady,
				fileVersionStatusProcessing,
			),
		)
	}
	return query.Order("created_at DESC").Order("id DESC")
}

func nextFileVersionNumber(tx *gorm.DB, fileID uint, currentVersion int) (int, error) {
	maxVersion := currentVersion

	var maxFileVersion int
	if err := tx.Model(&FileVersion{}).
		Where("file_id = ?", fileID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&maxFileVersion).Error; err != nil {
		return 0, err
	}
	if maxFileVersion > maxVersion {
		maxVersion = maxFileVersion
	}

	var maxDataVersion int
	if err := tx.Model(&FileData{}).
		Where("file_id = ?", fileID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&maxDataVersion).Error; err != nil {
		return 0, err
	}
	if maxDataVersion > maxVersion {
		maxVersion = maxDataVersion
	}

	if maxVersion < 0 {
		maxVersion = 0
	}

	return maxVersion + 1, nil
}

func ensureFileVersionTransitionIdleWithDB(db *gorm.DB, fileID uint) error {
	var blockedVersion FileVersion
	err := db.
		Where("file_id = ? AND reconciliation_status = ?", fileID, fileVersionStatusProcessing).
		Order("version DESC").
		First(&blockedVersion).Error
	if err == nil {
		return &FileVersionTransitionError{
			FileID: fileID,
			State:  blockedVersion.ReconciliationStatus,
		}
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return nil
}

func lockFileForVersionTransition(tx *gorm.DB, fileID uint) (File, error) {
	var file File
	err := tx.
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", fileID).
		First(&file).Error
	if err != nil {
		return File{}, err
	}
	return file, nil
}

func (fs *FileService) ensureFileVersionTransitionIdle(file File) error {
	if err := EnsureFileVersionTransitionIdleByID(fs.DB, file.ID); err != nil {
		var transitionErr *FileVersionTransitionError
		if errors.As(err, &transitionErr) {
			transitionErr.Filename = file.Filename
		}
		return err
	}
	return nil
}

func (fs *FileService) enqueueVersionTransitionJob(tx *gorm.DB, fileID uint, fileVersionID uint, sourceVersion int, targetVersion int, materializedVersion int, requestedBy uint, operation string) error {
	now := time.Now()
	payloadJSON, err := json.Marshal(map[string]any{
		"file_id":              fileID,
		"file_version_id":      fileVersionID,
		"source_version":       sourceVersion,
		"target_version":       targetVersion,
		"materialized_version": materializedVersion,
		"requested_by":         requestedBy,
		"operation":            operation,
	})
	if err != nil {
		return err
	}

	job := FileVersionReconciliationJob{
		FileID:              fileID,
		FileVersionID:       fileVersionID,
		SourceVersion:       sourceVersion,
		TargetVersion:       targetVersion,
		MaterializedVersion: materializedVersion,
		RequestedBy:         requestedBy,
		Operation:           operation,
		Status:              reconciliationJobStatusPending,
		Attempts:            0,
		LastError:           "",
		Payload:             datatypes.JSON(payloadJSON),
		AvailableAt:         now,
	}

	return tx.Create(&job).Error
}

func RunVersionReconciliationJobs(db *gorm.DB, options VersionReconciliationRunOptions) (*VersionReconciliationRunResult, error) {
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}
	if err := recoverStaleVersionReconciliationJobs(db); err != nil {
		return nil, err
	}

	maxJobs := options.MaxJobs
	if maxJobs <= 0 {
		maxJobs = defaultVersionReconciliationMaxJobs
	}

	result := &VersionReconciliationRunResult{}
	for i := 0; i < maxJobs; i++ {
		job, ok, err := claimNextVersionReconciliationJob(db)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}

		result.Claimed++
		if err := processVersionReconciliationJobSafely(db, job); err != nil {
			retryable, markErr := markVersionReconciliationJobFailure(db, job, err)
			if markErr != nil {
				return nil, markErr
			}
			if retryable {
				result.Retried++
			} else {
				result.Failed++
			}
			continue
		}

		if err := markVersionReconciliationJobCompleted(db, job.ID); err != nil {
			return nil, err
		}
		result.Completed++
	}

	return result, nil
}

func recoverStaleVersionReconciliationJobs(db *gorm.DB) error {
	staleBefore := time.Now().Add(-defaultVersionReconciliationLeaseTTL)

	var processingJobs []FileVersionReconciliationJob
	if err := db.
		Where("status = ?", reconciliationJobStatusProcessing).
		Order("id ASC").
		Find(&processingJobs).Error; err != nil {
		return err
	}

	for i := range processingJobs {
		if !isVersionReconciliationJobStale(&processingJobs[i], staleBefore) {
			continue
		}
		if err := recoverStaleVersionReconciliationJob(db, &processingJobs[i], staleBefore); err != nil {
			return err
		}
	}

	return nil
}

func recoverStaleVersionReconciliationJob(db *gorm.DB, staleJob *FileVersionReconciliationJob, staleBefore time.Time) error {
	if staleJob == nil {
		return nil
	}

	now := time.Now()
	return db.Transaction(func(tx *gorm.DB) error {
		var current FileVersionReconciliationJob
		if err := tx.
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", staleJob.ID).
			First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		if current.Status != reconciliationJobStatusProcessing {
			return nil
		}

		if !isVersionReconciliationJobStale(&current, staleBefore) {
			return nil
		}

		recoveryMessage := fmt.Sprintf(
			"reconciliation worker lease expired after %s without completion; retrying automatically",
			defaultVersionReconciliationLeaseTTL,
		)
		jobUpdates := map[string]any{
			"last_error":   recoveryMessage,
			"started_at":   nil,
			"completed_at": nil,
		}
		versionUpdates := map[string]any{
			"reconciliation_error": recoveryMessage,
		}

		if current.Attempts >= maxVersionReconciliationAttempts {
			jobUpdates["status"] = reconciliationJobStatusFailed
			jobUpdates["completed_at"] = now
			versionUpdates["reconciliation_status"] = fileVersionStatusFailed
		} else {
			jobUpdates["status"] = reconciliationJobStatusPending
			jobUpdates["available_at"] = now
			versionUpdates["reconciliation_status"] = fileVersionStatusProcessing
		}

		if err := tx.Model(&FileVersionReconciliationJob{}).
			Where("id = ? AND status = ?", current.ID, reconciliationJobStatusProcessing).
			Updates(jobUpdates).Error; err != nil {
			return err
		}

		return tx.Model(&FileVersion{}).
			Where("id = ?", current.FileVersionID).
			Updates(versionUpdates).Error
	})
}

func claimNextVersionReconciliationJob(db *gorm.DB) (*FileVersionReconciliationJob, bool, error) {
	now := time.Now()
	var claimed *FileVersionReconciliationJob

	err := db.Transaction(func(tx *gorm.DB) error {
		var candidate FileVersionReconciliationJob
		err := tx.
			Where("status = ? AND available_at <= ?", reconciliationJobStatusPending, now).
			Order("available_at ASC, id ASC").
			First(&candidate).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		res := tx.Model(&FileVersionReconciliationJob{}).
			Where("id = ? AND status = ?", candidate.ID, reconciliationJobStatusPending).
			Updates(map[string]any{
				"status":     reconciliationJobStatusProcessing,
				"started_at": now,
				"attempts":   candidate.Attempts + 1,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}

		candidate.Status = reconciliationJobStatusProcessing
		candidate.Attempts++
		candidate.StartedAt = &now
		claimed = &candidate
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if claimed == nil {
		return nil, false, nil
	}
	return claimed, true, nil
}

func markVersionReconciliationJobCompleted(db *gorm.DB, jobID uint) error {
	now := time.Now()
	return db.Model(&FileVersionReconciliationJob{}).
		Where("id = ?", jobID).
		Updates(map[string]any{
			"status":       reconciliationJobStatusCompleted,
			"completed_at": now,
			"last_error":   "",
		}).Error
}

func markVersionReconciliationJobFailure(db *gorm.DB, job *FileVersionReconciliationJob, runErr error) (bool, error) {
	now := time.Now()
	retryable := job.Attempts < maxVersionReconciliationAttempts
	if errors.Is(runErr, ErrFileVersionTargetVersionConflict) {
		retryable = false
	}

	jobUpdates := map[string]any{
		"last_error": runErr.Error(),
	}
	versionUpdates := map[string]any{
		"reconciliation_error": runErr.Error(),
	}

	if retryable {
		jobUpdates["status"] = reconciliationJobStatusPending
		jobUpdates["available_at"] = now.Add(versionReconciliationRetryDelay(job.Attempts))
		versionUpdates["reconciliation_status"] = fileVersionStatusProcessing
	} else {
		jobUpdates["status"] = reconciliationJobStatusFailed
		jobUpdates["completed_at"] = now
		versionUpdates["reconciliation_status"] = fileVersionStatusFailed
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&FileVersionReconciliationJob{}).
			Where("id = ?", job.ID).
			Updates(jobUpdates).Error; err != nil {
			return err
		}
		return tx.Model(&FileVersion{}).
			Where("id = ?", job.FileVersionID).
			Updates(versionUpdates).Error
	})
	if err != nil {
		return false, err
	}
	return retryable, nil
}

func versionReconciliationRetryDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return defaultVersionReconciliationRetryDelay
	}
	delay := time.Duration(attempts) * defaultVersionReconciliationRetryDelay
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}

func processVersionReconciliationJobSafely(db *gorm.DB, job *FileVersionReconciliationJob) (err error) {
	heartbeat := startVersionReconciliationHeartbeat(db, job.ID)
	defer heartbeat.Stop()

	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic while processing file version reconciliation job %d: %v", job.ID, recovered)
		}
	}()

	return processVersionReconciliationJob(db, job)
}

func latestVersionReconciliationJobActivity(job *FileVersionReconciliationJob) time.Time {
	if job == nil {
		return time.Time{}
	}

	var latest time.Time
	if job.StartedAt != nil && !job.StartedAt.IsZero() && job.StartedAt.After(latest) {
		latest = *job.StartedAt
	}
	if !job.UpdatedAt.IsZero() && job.UpdatedAt.After(latest) {
		latest = job.UpdatedAt
	}
	if latest.IsZero() {
		latest = job.CreatedAt
	}
	return latest
}

func isVersionReconciliationJobStale(job *FileVersionReconciliationJob, staleBefore time.Time) bool {
	return !latestVersionReconciliationJobActivity(job).After(staleBefore)
}

func startVersionReconciliationHeartbeat(db *gorm.DB, jobID uint) *versionReconciliationHeartbeat {
	if db == nil || jobID == 0 {
		return &versionReconciliationHeartbeat{}
	}

	ctx, cancel := context.WithCancel(requestscope.Context(db))
	heartbeat := &versionReconciliationHeartbeat{
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(heartbeat.done)

		_ = touchVersionReconciliationHeartbeat(db, jobID)

		ticker := time.NewTicker(defaultVersionReconciliationHeartbeat)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = touchVersionReconciliationHeartbeat(db, jobID)
			}
		}
	}()

	return heartbeat
}

func (h *versionReconciliationHeartbeat) Stop() {
	if h == nil {
		return
	}
	if h.cancel != nil {
		h.cancel()
	}
	if h.done != nil {
		<-h.done
	}
}

func touchVersionReconciliationHeartbeat(db *gorm.DB, jobID uint) error {
	if db == nil || jobID == 0 {
		return nil
	}

	return db.Model(&FileVersionReconciliationJob{}).
		Where("id = ? AND status = ?", jobID, reconciliationJobStatusProcessing).
		Update("updated_at", time.Now()).
		Error
}

func processVersionReconciliationJob(db *gorm.DB, job *FileVersionReconciliationJob) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var file File
		if err := tx.Where("id = ?", job.FileID).First(&file).Error; err != nil {
			return err
		}
		if err := ensureTargetVersionHasNoConflicts(tx, job); err != nil {
			return err
		}

		var targetVersion FileVersion
		if err := tx.Where("id = ?", job.FileVersionID).First(&targetVersion).Error; err != nil {
			return err
		}

		if targetVersion.ReconciliationStatus == fileVersionStatusReady && file.Version == job.TargetVersion {
			return nil
		}

		sourceRows, err := loadSourceRowsForReconciliation(tx, job.FileID, job.SourceVersion)
		if err != nil {
			return err
		}
		targetRows, err := loadRowsForVersion(tx, job.FileID, job.TargetVersion)
		if err != nil {
			return err
		}

		cfg, err := loadVersionReconciliationConfig(tx, file)
		if err != nil {
			return err
		}

		links, carryForwardRows := buildVersionReconciliationPlan(sourceRows, targetRows, cfg)

		if err := applyVersionReconciliationPlan(tx, file, &targetVersion, job, sourceRows, targetRows, links, carryForwardRows); err != nil {
			return err
		}

		return nil
	})
}

func ensureTargetVersionHasNoConflicts(tx *gorm.DB, job *FileVersionReconciliationJob) error {
	if job == nil {
		return nil
	}

	var conflicts []uint
	if err := tx.Model(&FileVersion{}).
		Where("file_id = ? AND version = ? AND id <> ?", job.FileID, job.TargetVersion, job.FileVersionID).
		Order("id ASC").
		Pluck("id", &conflicts).Error; err != nil {
		return err
	}
	if len(conflicts) == 0 {
		return nil
	}

	return &FileVersionTargetVersionConflictError{
		FileID:         job.FileID,
		FileVersionID:  job.FileVersionID,
		TargetVersion:  job.TargetVersion,
		ConflictingIDs: conflicts,
	}
}

func loadSourceRowsForReconciliation(tx *gorm.DB, fileID uint, sourceVersion int) ([]versionRowDescriptor, error) {
	rows, err := loadRowsForVersion(tx, fileID, sourceVersion)
	if err != nil {
		return nil, err
	}

	rowByID := make(map[uint]versionRowDescriptor, len(rows))
	for _, row := range rows {
		rowByID[row.ID] = row
	}

	referencedIDs, err := collectReferencedRowIDs(tx, fileID)
	if err != nil {
		return nil, err
	}

	var missingIDs []uint
	for id := range referencedIDs {
		if _, ok := rowByID[id]; !ok {
			missingIDs = append(missingIDs, id)
		}
	}
	if len(missingIDs) > 0 {
		extraRows, err := loadRowsByID(tx, missingIDs)
		if err != nil {
			return nil, err
		}
		for _, row := range extraRows {
			rowByID[row.ID] = row
		}
	}

	approvedUpdates, err := loadApprovedFieldUpdatesByRow(tx, fileID)
	if err != nil {
		return nil, err
	}

	finalRows := make([]versionRowDescriptor, 0, len(rowByID))
	for _, row := range rowByID {
		row.HasLinkedState = referencedIDs[row.ID] || len(approvedUpdates[row.ID]) > 0
		row.ApprovedUpdates = approvedUpdates[row.ID]
		finalRows = append(finalRows, row)
	}

	sort.Slice(finalRows, func(i, j int) bool {
		if finalRows[i].Version != finalRows[j].Version {
			return finalRows[i].Version > finalRows[j].Version
		}
		return finalRows[i].ID < finalRows[j].ID
	})

	return finalRows, nil
}

func loadRowsForVersion(tx *gorm.DB, fileID uint, version int) ([]versionRowDescriptor, error) {
	var rows []storedVersionRow
	if err := tx.
		Table("file_data").
		Where("file_id = ? AND version = ?", fileID, version).
		Order("id ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	return buildVersionRowDescriptors(rows)
}

func loadRowsByID(tx *gorm.DB, ids []uint) ([]versionRowDescriptor, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var rows []storedVersionRow
	if err := tx.
		Table("file_data").
		Where("id IN ?", ids).
		Order("id ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	return buildVersionRowDescriptors(rows)
}

func buildVersionRowDescriptors(rows []storedVersionRow) ([]versionRowDescriptor, error) {
	out := make([]versionRowDescriptor, 0, len(rows))
	for _, row := range rows {
		rowMap, err := unmarshalStringMap(row.RowData)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", row.ID, err)
		}
		out = append(out, versionRowDescriptor{
			storedVersionRow: row,
			RowDataMap:       rowMap,
			ApprovedUpdates:  map[string]string{},
		})
	}
	return out, nil
}

func collectReferencedRowIDs(tx *gorm.DB, fileID uint) (map[uint]bool, error) {
	referenced := make(map[uint]bool)
	pluckIntoSet := func(query *gorm.DB, dest *[]int64) error {
		if err := query.Pluck("row_id", dest).Error; err != nil {
			return err
		}
		for _, raw := range *dest {
			if raw > 0 {
				referenced[uint(raw)] = true
			}
		}
		return nil
	}

	var ids []int64
	if err := pluckIntoSet(tx.Table("file_edit_request").Where("file_id = ? AND row_id > 0", fileID), &ids); err != nil {
		return nil, err
	}
	ids = ids[:0]
	if err := pluckIntoSet(tx.Table("file_edit_request_details").Where("file_id = ? AND row_id > 0", fileID), &ids); err != nil {
		return nil, err
	}
	ids = ids[:0]
	if err := pluckIntoSet(tx.Table("file_edit_request_photos").Where("file_id = ? AND row_id > 0", fileID), &ids); err != nil {
		return nil, err
	}
	ids = ids[:0]
	if tx.Migrator().HasTable("form_submissions") {
		if err := pluckIntoSet(tx.Table("form_submissions").Where("file_id = ? AND row_id > 0", fileID), &ids); err != nil {
			return nil, err
		}
	}
	ids = ids[:0]
	if tx.Migrator().HasTable((AchieverStory{}).TableName()) {
		if err := pluckIntoSet(tx.Table((AchieverStory{}).TableName()).Where("file_id = ? AND row_id > 0", fileID), &ids); err != nil {
			return nil, err
		}
	}

	return referenced, nil
}

func loadApprovedFieldUpdatesByRow(tx *gorm.DB, fileID uint) (map[uint]map[string]string, error) {
	var rows []sourceApprovedDetailRow
	err := tx.Table("file_edit_request fer").
		Select(`
			d.row_id,
			d.field_name,
			COALESCE(d.new_value, '') AS new_value,
			d.request_id,
			fer.created_at,
			d.id AS detail_id,
			fer.is_edited
		`).
		Joins("JOIN file_edit_request_details d ON d.request_id = fer.request_id").
		Where("fer.file_id = ? AND fer.status = ?", fileID, fileEditRequestStatusCompleted).
		Where("d.row_id > 0").
		Where("LOWER(TRIM(COALESCE(d.status, 'approved'))) = ?", "approved").
		Order("fer.created_at ASC, fer.request_id ASC, d.id ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make(map[uint]map[string]string)
	for _, row := range rows {
		rowID := uint(row.RowID)
		if rowID == 0 {
			continue
		}
		if out[rowID] == nil {
			out[rowID] = make(map[string]string)
		}
		out[rowID][strings.TrimSpace(row.FieldName)] = strings.TrimSpace(row.NewValue)
	}
	return out, nil
}

func loadVersionReconciliationConfig(tx *gorm.DB, file File) (*versionReconciliationConfig, error) {
	cfg := &versionReconciliationConfig{
		FieldHeaders: make(map[string]string),
		Weights: map[string]float64{
			"lastname":   35,
			"firstname":  30,
			"middlename": 10,
			"fullname":   15,
			"dob":        25,
			"school":     12,
			"community":  10,
			"location":   6,
		},
		Threshold: defaultVersionReconciliationThreshold,
		Margin:    defaultVersionReconciliationMargin,
	}

	if !tx.Migrator().HasTable((&dataconfig.DataConfig{}).TableName()) {
		return cfg, nil
	}

	var dc dataconfig.DataConfig
	err := tx.
		Where("file_id = ? AND is_active = ?", int64(file.ID), true).
		Order("updated_at DESC, id DESC").
		Take(&dc).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return cfg, nil
		}
		return nil, err
	}

	var raw any
	if err := json.Unmarshal(dc.Config, &raw); err != nil {
		return cfg, nil
	}

	collectReconciliationConfig(raw, cfg)
	return cfg, nil
}

func collectReconciliationConfig(raw any, cfg *versionReconciliationConfig) {
	switch v := raw.(type) {
	case map[string]any:
		for key, value := range v {
			normalizedKey := normalizeSearchValue(key)
			if header, ok := value.(string); ok {
				if logical := mapLogicalFieldKey(normalizedKey); logical != "" && strings.TrimSpace(header) != "" {
					cfg.FieldHeaders[logical] = strings.TrimSpace(header)
				}
			}

			if weightMap, ok := value.(map[string]any); ok && normalizedKey == "weights" {
				for weightKey, rawWeight := range weightMap {
					if logical := mapLogicalFieldKey(normalizeSearchValue(weightKey)); logical != "" {
						if parsed, ok := parseConfigNumber(rawWeight); ok {
							cfg.Weights[logical] = parsed
						}
					}
				}
			}

			if parsed, ok := parseConfigNumber(value); ok {
				switch normalizedKey {
				case "threshold", "score threshold", "minimum score", "match threshold":
					cfg.Threshold = parsed
				case "margin", "score margin", "minimum margin", "ambiguity margin":
					cfg.Margin = parsed
				}
			}

			collectReconciliationConfig(value, cfg)
		}
	case []any:
		for _, item := range v {
			collectReconciliationConfig(item, cfg)
		}
	}
}

func parseConfigNumber(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func mapLogicalFieldKey(key string) string {
	switch key {
	case "firstname", "first name", "first names", "given name", "given names":
		return "firstname"
	case "lastname", "last name", "last names", "surname", "surnames", "family name":
		return "lastname"
	case "middlename", "middle name", "middle names":
		return "middlename"
	case "fullname", "full name", "name":
		return "fullname"
	case "dob", "date of birth", "birth date", "birth year":
		return "dob"
	case "school", "schools", "residential school":
		return "school"
	case "community", "communities", "first nation", "reserve":
		return "community"
	case "location", "city", "town", "place":
		return "location"
	default:
		return ""
	}
}

func buildVersionReconciliationPlan(sourceRows []versionRowDescriptor, targetRows []versionRowDescriptor, cfg *versionReconciliationConfig) ([]rowLinkPlan, []versionRowDescriptor) {
	for i := range sourceRows {
		sourceRows[i].Features = buildRowMatchFeatures(sourceRows[i].RowDataMap, cfg)
	}
	for i := range targetRows {
		targetRows[i].Features = buildRowMatchFeatures(targetRows[i].RowDataMap, cfg)
	}

	sourceByID := make(map[uint]versionRowDescriptor, len(sourceRows))
	targetByID := make(map[uint]versionRowDescriptor, len(targetRows))
	for _, row := range sourceRows {
		sourceByID[row.ID] = row
	}
	for _, row := range targetRows {
		targetByID[row.ID] = row
	}

	sourceUsed := make(map[uint]bool)
	targetUsed := make(map[uint]bool)
	var links []rowLinkPlan

	exactTiers := []exactMatchTier{
		{Name: "exact_name_dob", Fields: []string{"lastname", "firstname", "dob"}},
		{Name: "exact_name_school", Fields: []string{"lastname", "firstname", "school"}},
		{Name: "exact_name_community", Fields: []string{"lastname", "firstname", "community"}},
		{Name: "exact_name_middle", Fields: []string{"lastname", "firstname", "middlename"}},
		{Name: "exact_name", Fields: []string{"lastname", "firstname"}},
		// Some files supply a single, unique name column rather than separate first
		// and last name columns. Match it directly only when the counterpart name
		// field is absent on both rows; exactMatchLinks still requires a unique
		// source/target pair for the value.
		{Name: "exact_firstname_only", Fields: []string{"firstname"}, RequiredEmptyFields: []string{"lastname"}},
		{Name: "exact_lastname_only", Fields: []string{"lastname"}, RequiredEmptyFields: []string{"firstname"}},
	}

	for _, tier := range exactTiers {
		for _, matched := range exactMatchLinks(sourceRows, targetRows, tier, sourceUsed, targetUsed) {
			links = append(links, matched)
			sourceUsed[matched.SourceID] = true
			targetUsed[matched.TargetID] = true
		}
	}

	blockIndex := make(map[string][]uint)
	for _, target := range targetRows {
		if targetUsed[target.ID] {
			continue
		}
		for _, key := range target.Features.BlockKeys {
			blockIndex[key] = append(blockIndex[key], target.ID)
		}
	}

	var proposals []rowMatchProposal
	for _, source := range sourceRows {
		if sourceUsed[source.ID] {
			continue
		}

		candidates := candidateTargetIDs(source, targetRows, targetUsed, blockIndex)
		scored := make([]rowMatchProposal, 0, len(candidates))
		for _, targetID := range candidates {
			target := targetByID[targetID]
			score := scoreRowMatch(source.Features, target.Features, cfg)
			if score <= 0 {
				continue
			}
			scored = append(scored, rowMatchProposal{
				SourceID: source.ID,
				TargetID: target.ID,
				Method:   "weighted_match",
				Score:    score,
			})
		}
		if len(scored) == 0 {
			continue
		}

		sort.Slice(scored, func(i, j int) bool {
			if scored[i].Score != scored[j].Score {
				return scored[i].Score > scored[j].Score
			}
			return scored[i].TargetID < scored[j].TargetID
		})

		best := scored[0]
		if best.Score < cfg.Threshold {
			continue
		}
		if len(scored) > 1 && (best.Score-scored[1].Score) < cfg.Margin {
			continue
		}
		proposals = append(proposals, best)
	}

	sort.Slice(proposals, func(i, j int) bool {
		if proposals[i].Score != proposals[j].Score {
			return proposals[i].Score > proposals[j].Score
		}
		return proposals[i].SourceID < proposals[j].SourceID
	})

	for _, proposal := range proposals {
		if sourceUsed[proposal.SourceID] || targetUsed[proposal.TargetID] {
			continue
		}
		sourceUsed[proposal.SourceID] = true
		targetUsed[proposal.TargetID] = true
		links = append(links, rowLinkPlan{
			SourceID: proposal.SourceID,
			TargetID: proposal.TargetID,
			Method:   proposal.Method,
			Score:    proposal.Score,
		})
	}

	var carryForward []versionRowDescriptor
	for _, source := range sourceRows {
		if sourceUsed[source.ID] {
			continue
		}
		if source.HasLinkedState {
			carryForward = append(carryForward, source)
		}
	}

	sort.Slice(links, func(i, j int) bool {
		if links[i].Score != links[j].Score {
			return links[i].Score > links[j].Score
		}
		return links[i].SourceID < links[j].SourceID
	})
	sort.Slice(carryForward, func(i, j int) bool {
		return carryForward[i].ID < carryForward[j].ID
	})

	return links, carryForward
}

func exactMatchLinks(sourceRows []versionRowDescriptor, targetRows []versionRowDescriptor, tier exactMatchTier, sourceUsed map[uint]bool, targetUsed map[uint]bool) []rowLinkPlan {
	sourceBuckets := make(map[string][]uint)
	targetBuckets := make(map[string][]uint)

	for _, source := range sourceRows {
		if sourceUsed[source.ID] {
			continue
		}
		key := buildExactMatchKeyForTier(source.Features, tier)
		if key == "" {
			continue
		}
		sourceBuckets[key] = append(sourceBuckets[key], source.ID)
	}
	for _, target := range targetRows {
		if targetUsed[target.ID] {
			continue
		}
		key := buildExactMatchKeyForTier(target.Features, tier)
		if key == "" {
			continue
		}
		targetBuckets[key] = append(targetBuckets[key], target.ID)
	}

	var links []rowLinkPlan
	for key, sourceIDs := range sourceBuckets {
		targetIDs := targetBuckets[key]
		if len(sourceIDs) != 1 || len(targetIDs) != 1 {
			continue
		}
		links = append(links, rowLinkPlan{
			SourceID: sourceIDs[0],
			TargetID: targetIDs[0],
			Method:   tier.Name,
			Score:    100,
		})
	}

	sort.Slice(links, func(i, j int) bool {
		return links[i].SourceID < links[j].SourceID
	})
	return links
}

func buildExactMatchKeyForTier(features rowMatchFeatures, tier exactMatchTier) string {
	for _, field := range tier.RequiredEmptyFields {
		if strings.TrimSpace(features.LogicalFields[field]) != "" {
			return ""
		}
	}
	return buildExactMatchKey(features, tier.Fields)
}

func buildExactMatchKey(features rowMatchFeatures, logicalFields []string) string {
	parts := make([]string, 0, len(logicalFields))
	for _, field := range logicalFields {
		value := strings.TrimSpace(features.LogicalFields[field])
		if value == "" {
			return ""
		}
		if field == "dob" {
			if parsed := normalizeDateForMatch(features.Dates[field]); parsed != "" {
				value = parsed
			}
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, "|")
}

func candidateTargetIDs(source versionRowDescriptor, targetRows []versionRowDescriptor, targetUsed map[uint]bool, blockIndex map[string][]uint) []uint {
	seen := make(map[uint]bool)
	var candidates []uint

	for _, blockKey := range source.Features.BlockKeys {
		for _, targetID := range blockIndex[blockKey] {
			if targetUsed[targetID] || seen[targetID] {
				continue
			}
			seen[targetID] = true
			candidates = append(candidates, targetID)
		}
	}

	if len(candidates) > 0 {
		return candidates
	}

	for _, target := range targetRows {
		if targetUsed[target.ID] {
			continue
		}
		candidates = append(candidates, target.ID)
	}
	return candidates
}

func buildRowMatchFeatures(rowMap map[string]string, cfg *versionReconciliationConfig) rowMatchFeatures {
	logicalFields := make(map[string]string)
	dates := make(map[string]*normalizedDate)
	rawFields := make(map[string]string)
	headerIndex := make(map[string]string)

	for key, value := range rowMap {
		normalizedHeader := normalizeSearchValue(key)
		if normalizedHeader == "" {
			continue
		}
		headerIndex[normalizedHeader] = key
		normalizedValue := normalizeSearchValue(value)
		if normalizedValue != "" {
			rawFields[normalizedHeader] = normalizedValue
		}
	}

	for _, logical := range []string{"firstname", "middlename", "lastname", "fullname", "dob", "school", "community", "location"} {
		rawValue := extractLogicalFieldValue(rowMap, headerIndex, logical, cfg)
		normalizedValue := normalizeSearchValue(rawValue)
		if normalizedValue != "" {
			logicalFields[logical] = normalizedValue
		}
		if logical == "dob" && strings.TrimSpace(rawValue) != "" {
			dates[logical] = inferDateHint(rawValue)
		}
	}

	if logicalFields["fullname"] == "" {
		fullNameParts := make([]string, 0, 3)
		for _, field := range []string{"firstname", "middlename", "lastname"} {
			if value := strings.TrimSpace(logicalFields[field]); value != "" {
				fullNameParts = append(fullNameParts, value)
			}
		}
		if len(fullNameParts) > 0 {
			logicalFields["fullname"] = strings.Join(fullNameParts, " ")
		}
	}

	var blockKeys []string
	first := logicalFields["firstname"]
	last := logicalFields["lastname"]
	school := logicalFields["school"]
	community := logicalFields["community"]
	fullName := logicalFields["fullname"]
	if first != "" && last != "" {
		blockKeys = append(blockKeys, "name|"+last+"|"+first)
	}
	if last != "" && school != "" {
		blockKeys = append(blockKeys, "school|"+last+"|"+school)
	}
	if last != "" && community != "" {
		blockKeys = append(blockKeys, "community|"+last+"|"+community)
	}
	if fullName != "" {
		blockKeys = append(blockKeys, "full|"+fullName)
	}

	return rowMatchFeatures{
		LogicalFields: logicalFields,
		RawFields:     rawFields,
		Dates:         dates,
		BlockKeys:     uniqueStrings(blockKeys),
	}
}

func extractLogicalFieldValue(rowMap map[string]string, headerIndex map[string]string, logical string, cfg *versionReconciliationConfig) string {
	candidates := make([]string, 0, 4)
	if header := strings.TrimSpace(cfg.FieldHeaders[logical]); header != "" {
		candidates = append(candidates, header)
	}
	candidates = append(candidates, logicalFieldAliases(logical)...)

	for _, candidate := range candidates {
		if value, ok := rowMap[candidate]; ok {
			return value
		}
		if key, ok := headerIndex[normalizeSearchValue(candidate)]; ok {
			return rowMap[key]
		}
	}
	return ""
}

func logicalFieldAliases(logical string) []string {
	switch logical {
	case "firstname":
		return []string{"First Names", "First Name", "Firstname", "Given Name", "Given Names"}
	case "middlename":
		return []string{"Middle Names", "Middle Name", "Middlename"}
	case "lastname":
		return []string{"Last Names", "Last Name", "Lastname", "Surname", "Surnames"}
	case "fullname":
		return []string{"Full Name", "Name"}
	case "dob":
		return []string{"DOB", "Date of Birth", "Birth Date", "Birth Year"}
	case "school":
		return []string{"School", "Schools", "Residential School"}
	case "community":
		return []string{"Community", "Communities", "First Nation", "Reserve"}
	case "location":
		return []string{"Location", "Place", "City", "Town"}
	default:
		return nil
	}
}

func scoreRowMatch(source rowMatchFeatures, target rowMatchFeatures, cfg *versionReconciliationConfig) float64 {
	score := 0.0

	score += scoreExactLogicalField(source, target, cfg, "lastname")
	score += scoreExactLogicalField(source, target, cfg, "firstname")
	score += scoreMiddleName(source, target, cfg)
	score += scoreDateField(source, target, cfg)
	score += scoreExactLogicalField(source, target, cfg, "school")
	score += scoreExactLogicalField(source, target, cfg, "community")
	score += scoreExactLogicalField(source, target, cfg, "location")

	fullNameWeight := cfg.Weights["fullname"]
	if fullNameWeight == 0 {
		fullNameWeight = 15
	}
	if source.LogicalFields["fullname"] != "" && source.LogicalFields["fullname"] == target.LogicalFields["fullname"] {
		score += fullNameWeight
	}

	score += scoreGenericSharedFields(source, target)

	if source.LogicalFields["lastname"] != "" && target.LogicalFields["lastname"] != "" && source.LogicalFields["lastname"] != target.LogicalFields["lastname"] {
		score -= 20
	}
	if source.LogicalFields["firstname"] != "" && target.LogicalFields["firstname"] != "" && source.LogicalFields["firstname"] != target.LogicalFields["firstname"] {
		score -= 15
	}
	if source.LogicalFields["school"] != "" && target.LogicalFields["school"] != "" && source.LogicalFields["school"] != target.LogicalFields["school"] {
		score -= 8
	}
	if source.LogicalFields["community"] != "" && target.LogicalFields["community"] != "" && source.LogicalFields["community"] != target.LogicalFields["community"] {
		score -= 6
	}
	if mismatchedDate(source.Dates["dob"], target.Dates["dob"]) {
		score -= 20
	}

	return score
}

func scoreExactLogicalField(source rowMatchFeatures, target rowMatchFeatures, cfg *versionReconciliationConfig, logical string) float64 {
	left := strings.TrimSpace(source.LogicalFields[logical])
	right := strings.TrimSpace(target.LogicalFields[logical])
	if left == "" || right == "" {
		return 0
	}
	if left != right {
		return 0
	}
	if weight := cfg.Weights[logical]; weight > 0 {
		return weight
	}
	return 0
}

func scoreMiddleName(source rowMatchFeatures, target rowMatchFeatures, cfg *versionReconciliationConfig) float64 {
	left := strings.TrimSpace(source.LogicalFields["middlename"])
	right := strings.TrimSpace(target.LogicalFields["middlename"])
	if left == "" || right == "" {
		return 0
	}

	weight := cfg.Weights["middlename"]
	if weight == 0 {
		weight = 10
	}
	if left == right {
		return weight
	}
	if firstToken(left) == firstToken(right) {
		return weight * 0.6
	}
	return 0
}

func scoreDateField(source rowMatchFeatures, target rowMatchFeatures, cfg *versionReconciliationConfig) float64 {
	left := source.Dates["dob"]
	right := target.Dates["dob"]
	if left == nil || right == nil {
		return 0
	}

	weight := cfg.Weights["dob"]
	if weight == 0 {
		weight = 25
	}

	leftExact, leftHasExact := exactDateValue(left)
	rightExact, rightHasExact := exactDateValue(right)
	if leftHasExact && rightHasExact && leftExact == rightExact {
		return weight
	}

	leftYear, leftHasYear := normalizedDateYear(left)
	rightYear, rightHasYear := normalizedDateYear(right)
	if leftHasYear && rightHasYear && leftYear == rightYear {
		return weight * 0.5
	}
	return 0
}

func scoreGenericSharedFields(source rowMatchFeatures, target rowMatchFeatures) float64 {
	score := 0.0
	for key, left := range source.RawFields {
		if left == "" {
			continue
		}
		if right, ok := target.RawFields[key]; ok && right == left {
			score += maxGenericFieldWeight
			if score >= maxGenericFieldScore {
				return maxGenericFieldScore
			}
		}
	}
	return score
}

func exactDateValue(d *normalizedDate) (string, bool) {
	if d == nil || strings.TrimSpace(d.Raw) == "" {
		return "", false
	}
	parsed, err := tryParseDate(d.Raw)
	if err != nil {
		return "", false
	}
	return parsed.UTC().Format("2006-01-02"), true
}

func normalizeDateForMatch(d *normalizedDate) string {
	if exact, ok := exactDateValue(d); ok {
		return exact
	}
	if year, ok := normalizedDateYear(d); ok {
		return strconv.Itoa(year)
	}
	return ""
}

func normalizedDateYear(d *normalizedDate) (int, bool) {
	if d == nil || d.LowerYear == nil {
		return 0, false
	}
	return *d.LowerYear, true
}

func mismatchedDate(left *normalizedDate, right *normalizedDate) bool {
	leftYear, leftOK := normalizedDateYear(left)
	rightYear, rightOK := normalizedDateYear(right)
	return leftOK && rightOK && leftYear != rightYear
}

func firstToken(value string) string {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func applyVersionReconciliationPlan(tx *gorm.DB, file File, targetVersion *FileVersion, job *FileVersionReconciliationJob, sourceRows []versionRowDescriptor, targetRows []versionRowDescriptor, links []rowLinkPlan, carryForwardRows []versionRowDescriptor) error {
	sourceByID := make(map[uint]versionRowDescriptor, len(sourceRows))
	targetByID := make(map[uint]versionRowDescriptor, len(targetRows))
	for _, source := range sourceRows {
		sourceByID[source.ID] = source
	}
	for _, target := range targetRows {
		targetByID[target.ID] = target
	}

	linkMap := make(map[uint]rowLinkPlan, len(links))
	targetUpdates := make(map[uint]map[string]string)
	columnFields := make(map[string]struct{})
	lineages := make([]FileRowLineage, 0, len(links)+len(carryForwardRows))

	for _, link := range links {
		linkMap[link.SourceID] = link
		source := sourceByID[link.SourceID]
		for field, value := range source.ApprovedUpdates {
			if strings.TrimSpace(field) == "" {
				continue
			}
			if targetUpdates[link.TargetID] == nil {
				targetUpdates[link.TargetID] = make(map[string]string)
			}
			targetUpdates[link.TargetID][field] = value
			columnFields[field] = struct{}{}
		}
		targetRowID := link.TargetID
		lineages = append(lineages, buildLineageRow(file.ID, job.SourceVersion, job.TargetVersion, source.ID, &targetRowID, "matched", link.Method, link.Score, source.ApprovedUpdates))
	}

	for targetID, updates := range targetUpdates {
		target := targetByID[targetID]
		for field := range target.RowDataMap {
			columnFields[field] = struct{}{}
		}
		rowJSON, err := marshalPatchedRow(target.RowDataMap, updates)
		if err != nil {
			return err
		}
		if err := tx.Model(&FileData{}).
			Where("id = ?", targetID).
			Updates(map[string]any{
				"row_data":    rowJSON,
				"updated_at":  time.Now(),
				"inserted_by": target.InsertedBy,
			}).Error; err != nil {
			return err
		}
	}

	relinkRowIDMap := make(map[uint]uint, len(links)+len(carryForwardRows))
	for _, link := range links {
		source := sourceByID[link.SourceID]
		if !source.HasLinkedState || link.SourceID == link.TargetID {
			continue
		}
		relinkRowIDMap[link.SourceID] = link.TargetID
	}

	for _, source := range carryForwardRows {
		rowMap := cloneStringMap(source.RowDataMap)
		for field, value := range source.ApprovedUpdates {
			rowMap[field] = value
			columnFields[field] = struct{}{}
		}
		for field := range rowMap {
			columnFields[field] = struct{}{}
		}

		rowJSON, err := marshalStringMap(rowMap)
		if err != nil {
			return err
		}

		newRow := FileData{
			FileID:     file.ID,
			RowData:    rowJSON,
			InsertedBy: source.InsertedBy,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
			Version:    job.TargetVersion,
		}
		if err := tx.Create(&newRow).Error; err != nil {
			return err
		}

		if source.HasLinkedState && source.ID != newRow.ID {
			relinkRowIDMap[source.ID] = newRow.ID
		}
		lineages = append(lineages, buildLineageRow(file.ID, job.SourceVersion, job.TargetVersion, source.ID, &newRow.ID, "carried_forward", "carried_forward", 0, source.ApprovedUpdates))
	}

	if err := bulkRelinkRowReferences(tx, file.ID, file.Filename, relinkRowIDMap); err != nil {
		return err
	}

	if err := replaceLineageRows(tx, file.ID, job.TargetVersion, lineages); err != nil {
		return err
	}

	fields := make([]string, 0, len(columnFields))
	for field := range columnFields {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	updatedColumnsOrder, err := appendFieldsToColumnsOrder(targetVersion.ColumnsOrder, fields)
	if err != nil {
		return err
	}

	var finalRows int64
	if err := tx.Model(&FileData{}).
		Where("file_id = ? AND version = ?", file.ID, job.TargetVersion).
		Count(&finalRows).Error; err != nil {
		return err
	}

	now := time.Now()
	if err := tx.Model(&FileVersion{}).
		Where("id = ?", targetVersion.ID).
		Updates(map[string]any{
			"rows":                  finalRows,
			"columns_order":         updatedColumnsOrder,
			"reconciliation_status": fileVersionStatusReady,
			"reconciliation_error":  "",
			"reconciled_at":         now,
		}).Error; err != nil {
		return err
	}

	return tx.Model(&File{}).
		Where("id = ?", file.ID).
		Updates(map[string]any{
			"description":   targetVersion.Description,
			"version":       job.TargetVersion,
			"rows":          finalRows,
			"size":          targetVersion.Size,
			"private":       targetVersion.Private,
			"columns_order": updatedColumnsOrder,
		}).Error
}

func buildLineageRow(fileID uint, sourceVersion int, targetVersion int, sourceRowID uint, targetRowID *uint, status string, method string, score float64, fields map[string]string) FileRowLineage {
	fieldNames := make([]string, 0, len(fields))
	for field := range fields {
		fieldNames = append(fieldNames, field)
	}
	sort.Strings(fieldNames)

	debugJSON, _ := json.Marshal(debugLineagePayload{
		Method:        method,
		SourceVersion: sourceVersion,
		TargetVersion: targetVersion,
		Score:         score,
		Matched:       targetRowID != nil,
		TargetRowID:   targetRowID,
		Fields:        fieldNames,
	})

	return FileRowLineage{
		FileID:        fileID,
		TargetVersion: targetVersion,
		SourceVersion: sourceVersion,
		SourceRowID:   sourceRowID,
		TargetRowID:   targetRowID,
		Status:        status,
		Method:        method,
		Score:         score,
		DebugJSON:     datatypes.JSON(debugJSON),
	}
}

func replaceLineageRows(tx *gorm.DB, fileID uint, targetVersion int, rows []FileRowLineage) error {
	if err := tx.Where("file_id = ? AND target_version = ?", fileID, targetVersion).Delete(&FileRowLineage{}).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.Create(&rows).Error
}

func bulkRelinkRowReferences(tx *gorm.DB, fileID uint, fileName string, rowIDMap map[uint]uint) error {
	if len(rowIDMap) == 0 {
		return nil
	}

	sourceIDs := sortedRelinkSourceIDs(rowIDMap)
	for start := 0; start < len(sourceIDs); start += defaultVersionReconciliationRelinkBatch {
		end := start + defaultVersionReconciliationRelinkBatch
		if end > len(sourceIDs) {
			end = len(sourceIDs)
		}

		batch := sourceIDs[start:end]
		if err := bulkRelinkTableRows(tx, "file_edit_request", fileID, "", batch, rowIDMap); err != nil {
			return err
		}
		if err := bulkRelinkTableRows(tx, "file_edit_request_details", fileID, "", batch, rowIDMap); err != nil {
			return err
		}
		if err := bulkRelinkTableRows(tx, "file_edit_request_photos", fileID, "", batch, rowIDMap); err != nil {
			return err
		}
		if tx.Migrator().HasTable("form_submissions") {
			if err := bulkRelinkTableRows(tx, "form_submissions", fileID, fileName, batch, rowIDMap); err != nil {
				return err
			}
		}
		if tx.Migrator().HasTable((AchieverStory{}).TableName()) {
			if err := bulkRelinkTableRows(tx, (AchieverStory{}).TableName(), fileID, "", batch, rowIDMap); err != nil {
				return err
			}
		}
	}

	return nil
}

func sortedRelinkSourceIDs(rowIDMap map[uint]uint) []uint {
	sourceIDs := make([]uint, 0, len(rowIDMap))
	for sourceID, targetID := range rowIDMap {
		if sourceID == 0 || targetID == 0 || sourceID == targetID {
			continue
		}
		sourceIDs = append(sourceIDs, sourceID)
	}
	sort.Slice(sourceIDs, func(i, j int) bool {
		return sourceIDs[i] < sourceIDs[j]
	})
	return sourceIDs
}

func bulkRelinkTableRows(tx *gorm.DB, tableName string, fileID uint, fileName string, sourceIDs []uint, rowIDMap map[uint]uint) error {
	if len(sourceIDs) == 0 {
		return nil
	}

	caseParts := make([]string, 0, len(sourceIDs))
	inPlaceholders := make([]string, 0, len(sourceIDs))
	args := make([]any, 0, len(sourceIDs)*3+2)
	for _, sourceID := range sourceIDs {
		targetID := rowIDMap[sourceID]
		if targetID == 0 || sourceID == targetID {
			continue
		}
		caseParts = append(caseParts, "WHEN ? THEN ?")
		inPlaceholders = append(inPlaceholders, "?")
		args = append(args, int64(sourceID), int64(targetID))
	}
	if len(caseParts) == 0 {
		return nil
	}

	setClauses := []string{
		fmt.Sprintf("row_id = CASE row_id %s ELSE row_id END", strings.Join(caseParts, " ")),
	}
	if strings.EqualFold(tableName, "form_submissions") {
		setClauses = append(setClauses, "file_name = ?")
		args = append(args, fileName)
	}

	args = append(args, int64(fileID))
	for _, sourceID := range sourceIDs {
		args = append(args, int64(sourceID))
	}

	query := fmt.Sprintf(
		`UPDATE "%s" SET %s WHERE file_id = ? AND row_id IN (%s)`,
		tableName,
		strings.Join(setClauses, ", "),
		strings.Join(inPlaceholders, ", "),
	)
	return tx.Exec(query, args...).Error
}

func relinkRowReferences(tx *gorm.DB, fileID uint, sourceRowID uint, targetRowID uint, fileName string) error {
	if sourceRowID == 0 || targetRowID == 0 || sourceRowID == targetRowID {
		return nil
	}

	if err := tx.Model(&FileEditRequest{}).
		Where("file_id = ? AND row_id = ?", fileID, sourceRowID).
		Update("row_id", targetRowID).Error; err != nil {
		return err
	}
	if err := tx.Model(&FileEditRequestDetails{}).
		Where("file_id = ? AND row_id = ?", fileID, sourceRowID).
		Update("row_id", targetRowID).Error; err != nil {
		return err
	}
	if err := tx.Model(&FileEditRequestPhoto{}).
		Where("file_id = ? AND row_id = ?", fileID, sourceRowID).
		Update("row_id", targetRowID).Error; err != nil {
		return err
	}
	if tx.Migrator().HasTable("form_submissions") {
		if err := tx.Table("form_submissions").
			Where("file_id = ? AND row_id = ?", fileID, sourceRowID).
			Updates(map[string]any{
				"row_id":    targetRowID,
				"file_name": fileName,
			}).Error; err != nil {
			return err
		}
	}
	if tx.Migrator().HasTable((AchieverStory{}).TableName()) {
		if err := tx.Model(&AchieverStory{}).
			Where("file_id = ? AND row_id = ?", fileID, sourceRowID).
			Update("row_id", targetRowID).Error; err != nil {
			return err
		}
	}
	return nil
}

func appendFieldsToColumnsOrder(existing datatypes.JSON, fields []string) (datatypes.JSON, error) {
	columns, err := unmarshalColumnsOrder(existing)
	if err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		columns = make([]string, 0, len(fields))
	}

	seen := make(map[string]bool, len(columns))
	for _, col := range columns {
		seen[col] = true
	}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || seen[field] {
			continue
		}
		columns = append(columns, field)
		seen[field] = true
	}
	return marshalColumnsOrder(columns)
}

func marshalPatchedRow(base map[string]string, updates map[string]string) (datatypes.JSON, error) {
	rowMap := cloneStringMap(base)
	for field, value := range updates {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		rowMap[field] = value
	}
	return marshalStringMap(rowMap)
}

func cloneStringMap(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func unmarshalStringMap(raw datatypes.JSON) (map[string]string, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal row_data: %w", err)
	}

	out := make(map[string]string, len(payload))
	for key, value := range payload {
		out[key] = stringifyRowValue(value)
	}
	return out, nil
}

func marshalStringMap(row map[string]string) (datatypes.JSON, error) {
	jsonBytes, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(jsonBytes), nil
}

func syncCurrentVersionMetadata(tx *gorm.DB, currentFile File, fields []string) error {
	updatedColumnsOrder, err := appendFieldsToColumnsOrder(currentFile.ColumnsOrder, fields)
	if err != nil {
		return err
	}

	var rowCount int64
	if err := tx.Model(&FileData{}).
		Where("file_id = ? AND version = ?", currentFile.ID, currentFile.Version).
		Count(&rowCount).Error; err != nil {
		return err
	}

	if err := tx.Model(&FileVersion{}).
		Where("file_id = ? AND version = ?", currentFile.ID, currentFile.Version).
		Updates(map[string]any{
			"rows":          rowCount,
			"columns_order": updatedColumnsOrder,
		}).Error; err != nil {
		return err
	}

	fileUpdate := tx.Model(&File{}).
		Where("id = ?", currentFile.ID).
		Updates(map[string]any{
			"rows":          rowCount,
			"columns_order": updatedColumnsOrder,
		})
	if fileUpdate.Error != nil {
		return fileUpdate.Error
	}
	return nil
}
