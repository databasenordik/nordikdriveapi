package file

import (
	"fmt"
	"mime/multipart"
	"strings"

	"gorm.io/gorm"
	"nordik-drive-api/internal/activity"
	"nordik-drive-api/internal/requestscope"
)

func (fs *FileService) auditEvent(tx *gorm.DB, action, level, message string, filename *string, metadata any) error {
	if fs.Audit == nil {
		return nil
	}
	actor := activity.ActorFrom(requestscope.Context(tx))
	var uid *uint
	if actor.ID > 0 {
		uid = &actor.ID
	}
	return fs.Audit.Put(tx, activity.Log{Level: level, Service: "file", UserID: uid, Action: action, Message: message, Filename: filename, Communities: actor.Communities}, metadata)
}
func (fs *FileService) auditMutation(action, level, message string, filename *string, metadata any, run func(*FileService) error) error {
	return fs.DB.Transaction(func(tx *gorm.DB) error {
		copy := *fs
		copy.DB = tx
		copy.Audit = nil
		if err := run(&copy); err != nil {
			return err
		}
		return fs.auditEvent(tx, action, level, message, filename, metadata)
	})
}
func (fs *FileService) auditUpload(files []*multipart.FileHeader, input FileUploadInput, userID uint) ([]File, error) {
	if err := validateUploadInputCounts(len(files), input); err != nil {
		return nil, err
	}
	var result []File
	var filename *string
	if len(input.FileNames) > 0 {
		filename = &input.FileNames[0]
	}
	err := fs.auditMutation("UPLOAD_FILE", "INFO", fmt.Sprintf("Files uploaded: %s", strings.Join(input.FileNames, ",")), filename, nil, func(copy *FileService) error {
		var err error
		result, err = copy.SaveFilesMultipart(files, input, userID)
		return err
	})
	return result, err
}
func (fs *FileService) auditNormalization(filename string, version int) (*NormalizationSyncResult, error) {
	var result *NormalizationSyncResult
	err := fs.DB.Transaction(func(tx *gorm.DB) error {
		copy := *fs
		copy.DB = tx
		copy.Audit = nil
		var err error
		result, err = copy.SyncNormalizedFileData(filename, version)
		if err != nil {
			return err
		}
		if result == nil {
			return nil
		}
		var name *string
		if strings.TrimSpace(filename) != "" {
			name = &filename
		}
		return fs.auditEvent(tx, "SYNC_NORMALIZED_FILE", "INFO", fmt.Sprintf("Normalized file synchronized : %s", syncNormalizedLogTarget(filename, version)), name, result)
	})
	return result, err
}
func (fs *FileService) auditFileState(id string, deleted bool) (File, error) {
	var file File
	err := fs.DB.Transaction(func(tx *gorm.DB) error {
		copy := *fs
		copy.DB = tx
		copy.Audit = nil
		var err error
		action, level, prefix := "RESTORE_FILE", "INFO", "File restored : "
		if deleted {
			file, err = copy.DeleteFile(id)
			action, level, prefix = "DELETE_FILE", "WARN", "File deleted : "
		} else {
			file, err = copy.ResetFile(id)
		}
		if err != nil {
			return err
		}
		return fs.auditEvent(tx, action, level, prefix+file.Filename, &file.Filename, nil)
	})
	return file, err
}
