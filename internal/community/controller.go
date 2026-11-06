package community

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type CommunityController struct {
	CommunityService *CommunityService
}

func (cc *CommunityController) GetAllCommunities(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	communities, err := cc.CommunityService.GetAllCommunities()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":     "Communities fetched successfully",
		"communities": communities,
	})
}

func (cc *CommunityController) AddCommunities(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	var req struct {
		Communities []string `json:"communities" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	err := cc.CommunityService.AddCommunities(req.Communities)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Communities added successfully",
	})
}
