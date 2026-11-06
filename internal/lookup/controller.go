package lookup

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type LookupController struct {
	Service LookupServiceAPI
}

func (lc *LookupController) GetAllProvinces(c *gin.Context) {
	lc = lc.withContext(c.Request.Context())
	provinces, err := lc.Service.GetAllProvinces()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, provinces)
}

func (lc *LookupController) GetDaySchoolsByProvince(c *gin.Context) {
	lc = lc.withContext(c.Request.Context())
	provinceIDStr := strings.TrimSpace(c.Param("province"))
	provinceID, err := strconv.Atoi(provinceIDStr)
	if err != nil || provinceID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid province id is required"})
		return
	}

	daySchools, err := lc.Service.GetDaySchoolsByProvince(provinceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, daySchools)
}

func (lc *LookupController) GetIndianHospitalsByProvince(c *gin.Context) {
	lc = lc.withContext(c.Request.Context())
	provinceIDStr := strings.TrimSpace(c.Param("province"))
	provinceID, err := strconv.Atoi(provinceIDStr)
	if err != nil || provinceID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid province id is required"})
		return
	}

	hospitals, err := lc.Service.GetIndianHospitalsByProvince(provinceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, hospitals)
}
