package formsubmission

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	filehelper "nordik-drive-api/internal/file"
)

type FormSubmissionController struct {
	FormSubmissionService FormSubmissionServiceInterface
}

// GET /api/form/answers?id=...
func (cc *FormSubmissionController) GetFormSubmission(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	id, err := parseRequiredInt64Query(c.Query("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid id is required"})
		return
	}
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid id is required"})
		return
	}

	res, err := cc.FormSubmissionService.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, res)
}

// GET /api/form/answers/active?row_id=...&form_key=...&file_id=...
func (cc *FormSubmissionController) GetActiveFormSubmission(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	rowID, err := parseRequiredInt64Query(c.Query("row_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid row_id is required"})
		return
	}

	formKey := strings.TrimSpace(c.Query("form_key"))
	if formKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "form_key is required"})
		return
	}

	var fileIDPtr *int64
	fileIDStr := strings.TrimSpace(c.Query("file_id"))
	if fileIDStr != "" {
		fileID, err := strconv.ParseInt(fileIDStr, 10, 64)
		if err != nil || fileID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file_id"})
			return
		}
		fileIDPtr = &fileID
	}

	res, err := cc.FormSubmissionService.GetActiveByRowAndForm(rowID, formKey, fileIDPtr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if res == nil || !res.Found {
		c.JSON(http.StatusOK, gin.H{"data": nil})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": res})
}

// POST /api/form/answers
func (cc *FormSubmissionController) SaveFormSubmission(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
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

	uid := int(userID)

	var req SaveFormSubmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	res, err := cc.FormSubmissionService.Upsert(&req, uid)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, filehelper.ErrFileVersionTransitionInProgress) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, res)
}

func (cc *FormSubmissionController) GetUpload(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
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
	idParam := strings.TrimSpace(c.Param("id"))
	id, err := strconv.Atoi(idParam)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	data, contentType, filename, err := cc.FormSubmissionService.GetUploadBytes(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	disposition := "inline"
	if !strings.HasPrefix(contentType, "image/") && contentType != "application/pdf" {
		disposition = "attachment"
	}

	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, filename))
	c.Data(http.StatusOK, contentType, data)
}

func (cc *FormSubmissionController) SearchFormSubmissions(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
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

	var req SearchFormSubmissionsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	page := req.Page
	if page < 1 {
		page = 1
	}

	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	trimOpt := func(p **string) {
		if p == nil || *p == nil {
			return
		}
		v := strings.TrimSpace(**p)
		if v == "" {
			*p = nil
			return
		}
		*p = &v
	}

	normalizeStatusArray := func(values []string) []string {
		if len(values) == 0 {
			return nil
		}

		seen := make(map[string]struct{}, len(values))
		out := make([]string, 0, len(values))

		for _, v := range values {
			s := strings.TrimSpace(v)
			if s == "" {
				continue
			}

			if _, exists := seen[s]; exists {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}

		if len(out) == 0 {
			return nil
		}

		return out
	}

	trimOpt(&req.FormKey)
	trimOpt(&req.FirstName)
	trimOpt(&req.LastName)
	req.Status = normalizeStatusArray(req.Status)

	res, err := cc.FormSubmissionService.SearchSubmissions(c.Request.Context(), req, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, res)
}

func parseRequiredInt64Query(v string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
}

func (cc *FormSubmissionController) GetFormsByFileID(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	fileID, err := parseRequiredInt64Query(c.Query("file_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid file_id is required"})
		return
	}

	res, err := cc.FormSubmissionService.GetFormsByFileID(fileID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, res)
}

func (cc *FormSubmissionController) ReviewFormSubmission(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
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

	uid := int(userID)

	var req ReviewFormSubmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	res, err := cc.FormSubmissionService.ReviewSubmission(&req, uid)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidReviewRequest):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, ErrFormSubmissionNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, ErrUploadNotFoundForSubmission):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, res)
}

func (cc *FormSubmissionController) SearchMyFormSubmissions(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
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

	userID := int(userIDFloat)

	var req SearchFormSubmissionsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	page := req.Page
	if page < 1 {
		page = 1
	}

	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	trimOpt := func(p **string) {
		if p == nil || *p == nil {
			return
		}
		v := strings.TrimSpace(**p)
		if v == "" {
			*p = nil
			return
		}
		*p = &v
	}

	normalizeStatusArray := func(values []string) []string {
		if len(values) == 0 {
			return nil
		}

		seen := make(map[string]struct{}, len(values))
		out := make([]string, 0, len(values))

		for _, v := range values {
			s := strings.TrimSpace(v)
			if s == "" {
				continue
			}

			if _, exists := seen[s]; exists {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}

		if len(out) == 0 {
			return nil
		}

		return out
	}

	trimOpt(&req.FormKey)
	trimOpt(&req.FirstName)
	trimOpt(&req.LastName)
	req.Status = normalizeStatusArray(req.Status)

	res, err := cc.FormSubmissionService.SearchMySubmissions(
		c.Request.Context(),
		userID,
		req,
		page,
		pageSize,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, res)
}
