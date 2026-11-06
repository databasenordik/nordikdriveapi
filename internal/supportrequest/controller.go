package supportrequest

import (
	"errors"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type SupportRequestController struct {
	SupportRequestService SupportRequestServiceInterface
}

func (sc *SupportRequestController) Create(c *gin.Context) {
	sc = sc.withContext(c.Request.Context())
	if _, exists := c.Get("userID"); !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := supportRequestContextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	var req CreateSupportRequestRequest
	if err := c.ShouldBind(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var screenshotHeader *multipart.FileHeader
	fileHeader, err := c.FormFile("screenshot")
	switch {
	case err == nil:
		screenshotHeader = fileHeader
	case errors.Is(err, http.ErrMissingFile):
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	res, err := sc.SupportRequestService.Create(&req, userID, screenshotHeader)
	if err != nil {
		if errors.Is(err, ErrInvalidSupportRequest) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, res)
}

func (sc *SupportRequestController) ListMine(c *gin.Context) {
	sc = sc.withContext(c.Request.Context())
	userID, ok := supportRequestContextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	page, pageSize, err := supportRequestPagination(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	res, err := sc.SupportRequestService.ListMine(userID, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, res)
}

func (sc *SupportRequestController) ListForAdmin(c *gin.Context) {
	sc = sc.withContext(c.Request.Context())
	userID, ok := supportRequestContextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	page, pageSize, err := supportRequestPagination(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	res, err := sc.SupportRequestService.ListForAdmin(userID, page, pageSize)
	if err != nil {
		if errors.Is(err, ErrSupportRequestForbidden) {
			c.JSON(http.StatusForbidden, gin.H{"error": "admin access is required"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, res)
}

func (sc *SupportRequestController) Update(c *gin.Context) {
	sc = sc.withContext(c.Request.Context())
	userID, ok := supportRequestContextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid request ID is required"})
		return
	}

	var req UpdateSupportRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	res, err := sc.SupportRequestService.Update(id, &req, userID)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidSupportRequest):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, ErrSupportRequestForbidden):
			c.JSON(http.StatusForbidden, gin.H{"error": "admin access is required"})
		case errors.Is(err, ErrSupportRequestNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "support request not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, res)
}

func supportRequestContextUserID(c *gin.Context) (int, bool) {
	value, exists := c.Get("userID")
	if !exists {
		return 0, false
	}

	userIDFloat, ok := value.(float64)
	if !ok || userIDFloat <= 0 {
		return 0, false
	}

	return int(userIDFloat), true
}

func supportRequestPagination(c *gin.Context) (int, int, error) {
	page := 1
	pageSize := defaultSupportRequestPageSize

	if rawPage := c.Query("page"); rawPage != "" {
		parsed, err := strconv.Atoi(rawPage)
		if err != nil || parsed < 1 {
			return 0, 0, errors.New("page must be a positive integer")
		}
		page = parsed
	}
	if rawPageSize := c.Query("page_size"); rawPageSize != "" {
		parsed, err := strconv.Atoi(rawPageSize)
		if err != nil || parsed < 1 || parsed > maxSupportRequestPageSize {
			return 0, 0, errors.New("page_size must be between 1 and 100")
		}
		pageSize = parsed
	}

	return page, pageSize, nil
}
