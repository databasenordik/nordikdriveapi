package honour

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ServiceAPI interface {
	GetTodayByFilename(filename string) (*TodayResponse, error)
	RunDailyHonours() error
}

type Controller struct {
	Service ServiceAPI
}

func (hc *Controller) GetToday(c *gin.Context) {
	hc = hc.withContext(c.Request.Context())
	filename := strings.TrimSpace(c.Query("filename"))
	if filename == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "filename is required"})
		return
	}

	resp, err := hc.Service.GetTodayByFilename(filename)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

func (hc *Controller) RunDaily(c *gin.Context) {
	hc = hc.withContext(c.Request.Context())
	if hc.Service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "honour service not configured"})
		return
	}

	if err := hc.Service.RunDailyHonours(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
