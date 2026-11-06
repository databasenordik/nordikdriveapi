package logocontent

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type LogoContentController struct {
	Service LogoContentServiceAPI
}

func (lc *LogoContentController) GetHTMLByFileID(c *gin.Context) {
	lc = lc.withContext(c.Request.Context())
	fileIDStr := strings.TrimSpace(c.Param("fileId"))
	fileID64, err := strconv.ParseUint(fileIDStr, 10, 64)
	if err != nil || fileID64 == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid file id is required"})
		return
	}

	data, contentType, filename, err := lc.Service.GetHTMLByFileID(uint(fileID64))
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "logo content not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, sanitizeFilename(filename)))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self' data: https:; style-src 'unsafe-inline'; font-src https: data:; script-src 'none'; connect-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'")

	c.Data(http.StatusOK, contentType, data)
}

func sanitizeFilename(name string) string {
	if name == "" {
		return "logo-content.html"
	}

	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, `"`, "")
	return name
}
