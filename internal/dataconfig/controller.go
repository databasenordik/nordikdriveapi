package dataconfig

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type DataConfigController struct {
	DataConfigService DataConfigServiceAPI
}

// GET /data-config?file_name=...&last_modified=...
//
// last_modified should be the timestamp of the config you have in IndexedDB.
// Accepted formats:
// - RFC3339 / RFC3339Nano (recommended)
// - unix milliseconds (e.g., 1708451234567)
func (cc *DataConfigController) GetConfig(c *gin.Context) {
	fileName := strings.TrimSpace(c.Query("file_name"))
	if fileName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file_name is required"})
		return
	}

	clientLM, err := parseOptionalTime(c.Query("last_modified"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid last_modified (use RFC3339 or unix ms)"})
		return
	}

	res, err := cc.DataConfigService.GetByFileNameIfModified(fileName, clientLM)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "config not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	cfg := res.Config
	// Compute from JSON rather than trusting a stored checksum or timestamp.
	checksum := fmt.Sprintf("%x", sha256.Sum256(cfg.Config))
	c.Header("Cache-Control", "private, no-cache")

	c.Header("Last-Modified", cfg.UpdatedAt.UTC().Format(time.RFC3339Nano))
	c.Header("ETag", `"`+checksum+`"`)

	if c.Query("checksum") == checksum {
		c.JSON(http.StatusOK, gin.H{
			"not_modified": true,
			"file_id":      cfg.FileID,
			"file_name":    cfg.FileName,
			"version":      cfg.Version,
			"checksum":     checksum,
			"updated_at":   cfg.UpdatedAt,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"not_modified": false,
		"file_id":      cfg.FileID,
		"file_name":    cfg.FileName,
		"version":      cfg.Version,
		"checksum":     checksum,
		"updated_at":   cfg.UpdatedAt,
		"config":       configForResponse(cfg.Config),
	})
}

func parseOptionalTime(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "undefined") || strings.EqualFold(v, "null") {
		return nil, nil
	}

	// Try RFC3339/RFC3339Nano
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}

	// Try unix milliseconds
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		t := time.Unix(0, ms*int64(time.Millisecond))
		return &t, nil
	}

	return nil, strconv.ErrSyntax
}
