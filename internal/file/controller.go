package file

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"nordik-drive-api/internal/logs"
)

type FileController struct {
	FileService FileServicePort
	LogService  LogServicePort
}

type FileUploadInput struct {
	FileNames       []string `form:"filenames" binding:"required"`
	Private         []bool   `form:"private" binding:"required"`
	CommunityFilter []bool   `form:"community_filter" binding:"required"`
	Descriptions    []string `form:"descriptions"`
}

type ReplaceFileInput struct {
	Id uint `form:"id" binding:"required"`
}

func (fc *FileController) UploadFiles(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	// parse multipart form
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read form"})
		return
	}

	uploadedFiles := form.File["files"]
	if len(uploadedFiles) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no files uploaded"})
		return
	}

	// parse filenames array
	var input FileUploadInput
	if err := c.ShouldBind(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read filenames array"})
		return
	}

	if err := validateUploadInputCounts(len(uploadedFiles), input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	savedFiles, err := fc.FileService.SaveFilesMultipart(uploadedFiles, input, uint(userID))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	uid := uint(userID)

	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "file",
		UserID:      &uid,
		Action:      "UPLOAD_FILE",
		Message:     fmt.Sprintf("Files uploaded: %s", strings.Join(input.FileNames, ",")),
		Communities: communities,
		Filename:    &input.FileNames[0],
	}

	if err := fc.LogService.Log(log, nil); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "files uploaded successfully", "files": savedFiles})
}

func validateUploadInputCounts(fileCount int, input FileUploadInput) error {
	switch {
	case len(input.FileNames) != fileCount:
		return errors.New("files count and filenames array length mismatch")
	case len(input.Private) != fileCount:
		return errors.New("files count and private array length mismatch")
	case len(input.CommunityFilter) != fileCount:
		return errors.New("files count and community_filter array length mismatch")
	case len(input.Descriptions) > 0 && len(input.Descriptions) != fileCount:
		return errors.New("files count and descriptions array length mismatch")
	default:
		return nil
	}
}

func (fc *FileController) GetAllFiles(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(uint) // assuming middleware sets uint
	if !ok {
		// handle jwt float64 conversion case
		if f, ok2 := userIDVal.(float64); ok2 {
			userID = uint(f)
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
			return
		}
	}

	// fetch role
	role, err := fc.FileService.GetUserRole(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	files, err := fc.FileService.GetAllFiles(userID, role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Files fetched successfully",
		"files":   files,
	})
}

// type FileDataRequest struct {
// 	Filename string `json:"filename"`
// }

func (fc *FileController) GetFileData(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	fileName := c.Query("filename")
	versionStr := c.Query("version")
	version := 0
	if strings.TrimSpace(versionStr) != "" {
		parsedVersion, err := strconv.Atoi(versionStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version"})
			return
		}
		version = parsedVersion
	}

	if fileName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file name is required"})
		return
	}

	fileData, err := fc.FileService.GetFileData(fileName, version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if fileData == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	uid := uint(userID)

	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "file",
		UserID:      &uid,
		Action:      "ACCESS_FILE",
		Message:     fmt.Sprintf("File accessed : %s", fileName),
		Communities: communities,
		Filename:    &fileName,
	}

	if err := fc.LogService.Log(log, nil); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, fileData)
}

func (fc *FileController) GetNormalizedFileData(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	fileName := c.Query("filename")
	versionStr := c.Query("version")
	version, err := strconv.Atoi(versionStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version"})
		return
	}

	if fileName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file name is required"})
		return
	}

	normalizedRows, err := fc.FileService.GetNormalizedFileData(fileName, version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if normalizedRows == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	uid := uint(userID)
	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "file",
		UserID:      &uid,
		Action:      "ACCESS_NORMALIZED_FILE",
		Message:     fmt.Sprintf("Normalized file accessed : %s", fileName),
		Communities: communities,
		Filename:    &fileName,
	}

	if err := fc.LogService.Log(log, nil); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"rows": normalizedRows,
	})
}

func (fc *FileController) SyncNormalizedFileData(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	fileName := c.Query("filename")
	versionStr := c.Query("version")
	version := 0
	if strings.TrimSpace(versionStr) != "" {
		parsedVersion, err := strconv.Atoi(versionStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version"})
			return
		}
		version = parsedVersion
	}

	result, err := fc.FileService.SyncNormalizedFileData(fileName, version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if result == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	uid := uint(userID)
	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "file",
		UserID:      &uid,
		Action:      "SYNC_NORMALIZED_FILE",
		Message:     fmt.Sprintf("Normalized file synchronized : %s", syncNormalizedLogTarget(fileName, version)),
		Communities: communities,
	}
	if strings.TrimSpace(fileName) != "" {
		log.Filename = &fileName
	}

	if err := fc.LogService.Log(log, result); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Normalized file data synchronized successfully",
		"result":  result,
	})
}

func syncNormalizedLogTarget(fileName string, version int) string {
	fileName = strings.TrimSpace(fileName)
	switch {
	case fileName == "" && version <= 0:
		return "all active files (latest versions prioritized)"
	case fileName == "":
		return fmt.Sprintf("all active files for version %d", version)
	case version <= 0:
		return fmt.Sprintf("%s (latest version)", fileName)
	default:
		return fmt.Sprintf("%s (version %d)", fileName, version)
	}
}

func (fc *FileController) DeleteFile(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	fileID := c.Query("id")
	if fileID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file ID is required"})
		return
	}

	file, err := fc.FileService.DeleteFile(fileID)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	uid := uint(userID)

	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "WARN",
		Service:     "file",
		UserID:      &uid,
		Action:      "DELETE_FILE",
		Message:     fmt.Sprintf("File deleted : %s", file.Filename),
		Communities: communities,
		Filename:    &file.Filename,
	}

	if err := fc.LogService.Log(log, nil); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "File deleted successfully",
	})
}

func (fc *FileController) ResetFile(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	fileID := c.Query("id")
	if fileID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file ID is required"})
		return
	}

	file, err := fc.FileService.ResetFile(fileID)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	uid := uint(userID)

	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "file",
		UserID:      &uid,
		Action:      "RESTORE_FILE",
		Message:     fmt.Sprintf("File restored : %s", file.Filename),
		Communities: communities,
		Filename:    &file.Filename,
	}

	if err := fc.LogService.Log(log, nil); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "File restored successfully",
	})
}

func (fc *FileController) CreateAccess(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	var input []FileAccess
	if err := c.ShouldBind(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read data"})
		return
	}

	if err := fc.FileService.CreateAccess(input); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	uid := uint(userID)

	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "file",
		UserID:      &uid,
		Action:      "GRAND_FILE_ACCESS",
		Message:     "file access granted",
		Communities: communities,
	}

	if err := fc.LogService.Log(log, input); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "File access given successfully",
	})
}

func (fc *FileController) DeleteAccess(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	accessId := c.Query("id")

	if err := fc.FileService.DeleteAccess(accessId); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	uid := uint(userID)

	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "WARN",
		Service:     "file",
		UserID:      &uid,
		Action:      "REVOKE_FILE_ACCESS",
		Message:     "file access revoked",
		Communities: communities,
	}

	if err := fc.LogService.Log(log, nil); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "File access revoked successfully",
	})
}

type FileAccessWithUser struct {
	ID        uint   `json:"id"`
	UserID    uint   `json:"user_id"`
	FileID    uint   `json:"file_id"`
	FirstName string `json:"firstname" gorm:"column:firstname"`
	LastName  string `json:"lastname" gorm:"column:lastname"`
}

func (fc *FileController) GetAllAccess(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	_, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	fileId := c.Query("id")

	FileAccess, err := fc.FileService.GetFileAccess(fileId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if FileAccess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "access not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Access fetched successfully",
		"access":  FileAccess,
	})
}

func (fc *FileController) GetFileHistory(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	_, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	fileId := c.Query("id")

	fileHistory, err := fc.FileService.GetFileHistory(fileId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if fileHistory == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "history not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "File history fetched successfully",
		"history": fileHistory,
	})
}

func (fc *FileController) ReplaceFile(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no file uploaded"})
		return
	}

	var replaceFileInput ReplaceFileInput
	if err := c.ShouldBind(&replaceFileInput); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read file id"})
		return
	}

	err = fc.FileService.ReplaceFiles(file, replaceFileInput.Id, uint(userID))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrFileOperationBlocked) || errors.Is(err, ErrFileVersionTransitionInProgress) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	uid := uint(userID)

	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "file",
		UserID:      &uid,
		Action:      "REPLACE_FILE",
		Message:     fmt.Sprintf("File replace queued: %s", file.Filename),
		Communities: communities,
		Filename:    &file.Filename,
	}

	if err := fc.LogService.Log(log, nil); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "File replace queued for background reconciliation"})
}

func (fc *FileController) RevertFile(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	var input RevertFileInput
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := fc.FileService.RevertFile(input.Filename, input.Version, uint(userID)); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrFileOperationBlocked) || errors.Is(err, ErrFileVersionTransitionInProgress) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	uid := uint(userID)

	communitiesVal, exists := c.Get("communities")
	if !exists {
		communitiesVal = []string{}
	}

	communities, ok := communitiesVal.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid communities"})
		return
	}

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "file",
		UserID:      &uid,
		Action:      "REVERT_FILE",
		Message:     fmt.Sprintf("%s file revert queued to version %d", input.Filename, input.Version),
		Communities: communities,
		Filename:    &input.Filename,
	}

	if err := fc.LogService.Log(log, nil); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("file revert to version %d queued for background reconciliation", input.Version),
	})

}

func (fc *FileController) CreateEditRequest(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	var input EditRequestInput

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	request, err := fc.FileService.CreateEditRequest(input, uint(userID))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrFileVersionTransitionInProgress) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Edit request submitted",
		"request": request,
	})
}

// CreateAchieverStoryRequest accepts proposed text, an uploaded/linked video,
// PDF, or Word story. The story remains pending until an administrator reviews it.
func (fc *FileController) CreateAchieverStoryRequest(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	var input AchieverStoryRequestInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}
	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	request, err := fc.FileService.CreateAchieverStoryRequest(input, uint(userID))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrFileVersionTransitionInProgress) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Achiever story submitted for review",
		"request": request,
	})
}

func (fc *FileController) GetEditRequests(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	_, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	// Query params:
	// ?status=pending,completed
	// ?user_id=26
	statusRaw := strings.TrimSpace(c.Query("status"))
	userIDRaw := strings.TrimSpace(c.Query("user_id"))

	var statusPtr *string
	if statusRaw != "" {
		statusPtr = &statusRaw
	}

	var userIDPtr *uint
	if userIDRaw != "" {
		parsed, err := strconv.ParseUint(userIDRaw, 10, 32)
		if err != nil || parsed == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}
		u := uint(parsed)
		userIDPtr = &u
	}

	requests, err := fc.FileService.GetEditRequests(statusPtr, userIDPtr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"requests": requests})
}

func (fc *FileController) ReviewEditRequest(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	var input struct {
		RequestID     uint                       `json:"request_id"`
		Status        string                     `json:"status"`
		ReviewComment string                     `json:"reviewer_comment"`
		Updates       []FileEditRequestDetails   `json:"updates"`
		StoryReviews  []AchieverStoryReviewInput `json:"story_reviews"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input"})
		return
	}

	var reviewErr error
	if len(input.StoryReviews) > 0 {
		reviewErr = fc.FileService.ReviewAchieverStoryRequest(
			input.RequestID,
			input.ReviewComment,
			input.StoryReviews,
			uint(userID),
		)
	} else {
		reviewErr = fc.FileService.ReviewEditRequest(
			input.RequestID,
			input.Status,
			input.ReviewComment,
			input.Updates,
			uint(userID),
		)
	}
	if reviewErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": reviewErr.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Request completed successfully"})
}

// POST /api/file/photos/review
func (fc *FileController) ReviewPhotos(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userIDFloat, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	var input struct {
		Reviews []PhotoReviewInput `json:"reviews"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input"})
		return
	}

	if err := fc.FileService.ReviewPhotos(input.Reviews, uint(userIDFloat)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Photo review updated"})
}

func (fc *FileController) GetPhotosByRequest(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	requestIDParam := c.Param("requestId")
	requestIDInt, err := strconv.Atoi(requestIDParam)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request ID"})
		return
	}

	requestID := uint(requestIDInt)

	photos, err := fc.FileService.GetPhotosByRequest(requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"photos": photos,
	})
}

func (fc *FileController) GetDocsByRequest(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	requestIDParam := c.Param("requestId")
	requestIDInt, err := strconv.Atoi(requestIDParam)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request ID"})
		return
	}

	requestID := uint(requestIDInt)

	docs, err := fc.FileService.GetDocsByRequest(requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"docs": docs,
	})
}

func (fc *FileController) GetPhotosByRow(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	rowIDParam := c.Param("rowId")
	rowIDInt, err := strconv.Atoi(rowIDParam)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid row ID"})
		return
	}

	rowID := uint(rowIDInt)

	photos, err := fc.FileService.GetPhotosByRow(rowID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"photos": photos,
	})
}

func (fc *FileController) GetDocsByRow(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	rowIDParam := c.Param("rowId")
	rowIDInt, err := strconv.Atoi(rowIDParam)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid row ID"})
		return
	}

	rowID := uint(rowIDInt)

	docs, err := fc.FileService.GetDocsByRow(rowID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"docs": docs,
	})
}

// GetAchieverStoriesByRow returns approved survivor/achiever stories for one
// master-list row. Stories may be documents, text, or videos.
func (fc *FileController) GetAchieverStoriesByRow(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	rowID, err := strconv.ParseUint(c.Param("rowId"), 10, 64)
	if err != nil || rowID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid row ID"})
		return
	}

	stories, err := fc.FileService.GetAchieverStoriesByRow(uint(rowID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"achiever_stories": stories})
}

func (fc *FileController) GetPhoto(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	photoIDParam := c.Param("photoId")
	photoID, err := strconv.Atoi(photoIDParam)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid photo ID"})
		return
	}

	data, contentType, err := fc.FileService.GetPhotoBytes(uint(photoID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Return as image
	c.Data(http.StatusOK, contentType, data)
}

func (fc *FileController) GetDoc(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	docIDParam := c.Param("docId")
	docID, err := strconv.Atoi(docIDParam)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid doc ID"})
		return
	}

	data, contentType, filename, err := fc.FileService.GetDocBytes(uint(docID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Inline for PDF/images, attachment for others
	disposition := "attachment"
	if strings.HasPrefix(contentType, "image/") || contentType == "application/pdf" {
		disposition = "inline"
	}

	// Important headers
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, sanitizeFilename(filename)))
	c.Header("X-Content-Type-Options", "nosniff")

	c.Data(http.StatusOK, contentType, data)
}

func sanitizeFilename(name string) string {
	if name == "" {
		return "document"
	}
	// remove path separators and quotes
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, `"`, "")
	return name
}

func (fc *FileController) DownloadMediaByID(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	idStr := c.Param("id")
	id64, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || id64 == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	id := uint(id64)

	kind := strings.TrimSpace(c.Query("kind")) // optional: photo|doc|document
	handle, filename, contentType, _, err := fc.FileService.OpenMediaHandle(c.Request.Context(), id, kind)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer handle.Close()

	// If GCS content-type missing, sniff first bytes and stream
	if contentType == "" {
		buf := make([]byte, 512)
		n, _ := io.ReadFull(handle, buf)
		if n > 0 {
			contentType = http.DetectContentType(buf[:n])
		} else {
			contentType = "application/octet-stream"
		}

		c.Header("Content-Type", contentType)
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeFilename(filename)))
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")

		_, _ = c.Writer.Write(buf[:n])
		_, _ = io.Copy(c.Writer, handle)
		return
	}

	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeFilename(filename)))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")

	_, _ = io.Copy(c.Writer, handle)

}

// DownloadAchieverStory streams a stored PDF/document. Text and video stories
// are returned by GetAchieverStoriesByRow and are rendered directly by the UI.
func (fc *FileController) DownloadAchieverStory(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	id, err := strconv.ParseUint(c.Param("storyId"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid story id"})
		return
	}

	handle, filename, contentType, disposition, err := fc.FileService.OpenAchieverStoryHandle(c.Request.Context(), uint(id))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer handle.Close()

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if disposition == "" {
		disposition = "attachment"
	}

	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, sanitizeFilename(filename)))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")
	_, _ = io.Copy(c.Writer, handle)
}

// DownloadRequestedAchieverStory permits the story submitter and admins to
// preview a submitted document while it is still pending. The public story
// download route above remains approved-only.
func (fc *FileController) DownloadRequestedAchieverStory(c *gin.Context) {
	fc = fc.withContext(c.Request.Context())
	id, err := strconv.ParseUint(c.Param("storyId"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid story id"})
		return
	}

	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}
	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	role, err := fc.FileService.GetUserRole(uint(userID))
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "unable to verify user role"})
		return
	}
	isAdmin := strings.EqualFold(strings.TrimSpace(role), "admin")

	handle, filename, contentType, disposition, err := fc.FileService.OpenRequestedAchieverStoryHandle(
		c.Request.Context(), uint(id), uint(userID), isAdmin,
	)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "story is not available"})
		return
	}
	defer handle.Close()

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if disposition == "" {
		disposition = "attachment"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, sanitizeFilename(filename)))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")
	_, _ = io.Copy(c.Writer, handle)
}
