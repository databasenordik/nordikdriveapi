package supportschedule

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type Controller struct{ Service *Service }

func RegisterRoutes(r *gin.Engine, service *Service) {
	controller := &Controller{Service: service}
	group := r.Group("/api/support-schedule")
	group.Use(authRequired())
	{
		group.GET("/settings", controller.GetSettings)
		group.PUT("/settings", controller.UpdateSettings)
		group.GET("/availability", controller.ListAvailability)
		group.GET("/calendar", controller.ListCalendar)
		group.GET("/schedule", controller.ListSchedule)
		group.GET("/fairness", controller.ListFairnessStats)
		group.GET("/audit-log", controller.ListAuditLog)
		group.GET("/team", controller.ListSelectableStaff)
		group.GET("/team/manage", controller.ListTeam)
		group.PUT("/team/:userID", controller.SetTeamMember)
		group.GET("/requests", controller.ListRequests)
		group.POST("/requests", controller.CreateCall)
		group.PUT("/requests/:id/decision", controller.DecideRequest)
		group.PUT("/requests/:id/accept-alternative", controller.AcceptAlternative)
		group.PUT("/requests/:id/cancel", controller.CancelRequest)
		group.GET("/calls", controller.ListCalls)
		group.POST("/calls/:id/zoom/start", controller.StartZoomMeeting)
		group.PUT("/calls/:id/complete", controller.CompleteCall)
		group.PUT("/calls/:id/reassign", controller.ReassignCall)
		group.PUT("/schedule/:date/reassign", controller.ReassignDay)
		group.GET("/profile", controller.GetProfile)
		group.GET("/profile/availability", controller.ListUnavailability)
		group.POST("/profile/availability", controller.CreateUnavailability)
		group.PUT("/profile/availability/:id", controller.UpdateAvailability)
		group.DELETE("/profile/availability/:id", controller.DeleteAvailability)
		group.POST("/maintenance", controller.RunMaintenance)
	}
}

// authRequired is set by routes.go at package init to avoid packages that
// consume SupportScheduleService needing to know Gin's authentication details.
var authRequired = func() gin.HandlerFunc { return supportScheduleAuthMiddleware() }

func (cc *Controller) GetSettings(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	result, err := cc.Service.GetSettings()
	cc.respond(c, result, err)
}

func (cc *Controller) UpdateSettings(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	var input UpdateSettingsInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := cc.Service.UpdateSettings(actorID, input)
	cc.respond(c, result, err)
}

func (cc *Controller) ListAvailability(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	duration, err := optionalInt(c.Query("duration_minutes"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "duration_minutes must be a positive integer"})
		return
	}
	staffID, err := optionalUint(c.Query("staff_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "staff_id must be a positive integer"})
		return
	}
	result, err := cc.Service.ListAvailability(c.Query("date"), duration, staffID)
	cc.respond(c, result, err)
}

func (cc *Controller) ListCalendar(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	duration, err := optionalInt(c.Query("duration_minutes"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "duration_minutes must be a positive integer"})
		return
	}
	staffID, err := optionalUint(c.Query("staff_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "staff_id must be a positive integer"})
		return
	}
	result, err := cc.Service.ListCalendar(actorID, duration, staffID)
	cc.respond(c, result, err)
}

func (cc *Controller) ListSchedule(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	result, err := cc.Service.ListScheduleFor(actorID, c.Query("from"), c.Query("to"))
	cc.respond(c, result, err)
}

func (cc *Controller) ListFairnessStats(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	result, err := cc.Service.ListFairnessStats(actorID)
	cc.respond(c, result, err)
}

func (cc *Controller) ListAuditLog(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	result, err := cc.Service.ListAuditLog(actorID)
	cc.respond(c, result, err)
}
func (cc *Controller) ListSelectableStaff(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	result, err := cc.Service.ListSelectableStaff()
	cc.respond(c, result, err)
}

func (cc *Controller) ListTeam(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	result, err := cc.Service.ListTeam(actorID)
	cc.respond(c, result, err)
}

func (cc *Controller) SetTeamMember(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	userID, ok := pathID(c, "userID")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid team user ID"})
		return
	}
	var input TeamMemberInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := cc.Service.SetTeamMember(actorID, userID, input.IsActive)
	cc.respond(c, result, err)
}

func (cc *Controller) ListCalls(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	result, err := cc.Service.ListCalls(actorID, c.DefaultQuery("scope", "mine"))
	cc.respond(c, result, err)
}

func (cc *Controller) ListRequests(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	result, err := cc.Service.ListRequests(actorID, c.DefaultQuery("scope", "mine"))
	cc.respond(c, result, err)
}

func (cc *Controller) CreateCall(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	var input CreateCallInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	call, err := cc.Service.CreateCall(actorID, input)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, call)
}

func (cc *Controller) DecideRequest(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, callID, ok := actorAndCall(c)
	if !ok {
		return
	}
	var input RequestDecisionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := cc.Service.DecideRequest(actorID, callID, input)
	cc.respond(c, result, err)
}

func (cc *Controller) AcceptAlternative(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, requestID, ok := actorAndCall(c)
	if !ok {
		return
	}
	result, err := cc.Service.AcceptAlternative(actorID, requestID)
	cc.respond(c, result, err)
}

func (cc *Controller) CancelRequest(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, requestID, ok := actorAndCall(c)
	if !ok {
		return
	}
	result, err := cc.Service.CancelRequest(actorID, requestID)
	cc.respond(c, result, err)
}

func (cc *Controller) CompleteCall(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, callID, ok := actorAndCall(c)
	if !ok {
		return
	}
	var input CompleteCallInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := cc.Service.CompleteCall(actorID, callID, input)
	cc.respond(c, result, err)
}

func (cc *Controller) StartZoomMeeting(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, callID, ok := actorAndCall(c)
	if !ok {
		return
	}
	result, err := cc.Service.StartZoomMeeting(actorID, callID)
	cc.respond(c, result, err)
}

func (cc *Controller) ReassignCall(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, callID, ok := actorAndCall(c)
	if !ok {
		return
	}
	var input ReassignInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := cc.Service.ReassignCall(actorID, callID, input)
	cc.respond(c, result, err)
}

func (cc *Controller) ReassignDay(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	var input ReassignInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := cc.Service.ReassignDay(actorID, c.Param("date"), input)
	cc.respond(c, result, err)
}

func (cc *Controller) ListUnavailability(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	result, err := cc.Service.ListUnavailability(actorID)
	cc.respond(c, result, err)
}

func (cc *Controller) CreateUnavailability(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	var input CreateUnavailabilityInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	call, err := cc.Service.CreateUnavailability(actorID, input)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, call)
}

func (cc *Controller) UpdateAvailability(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	availabilityID, ok := pathID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid availability ID"})
		return
	}
	var input UpdateAvailabilityInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := cc.Service.UpdateAvailability(actorID, availabilityID, input)
	cc.respond(c, result, err)
}

func (cc *Controller) DeleteAvailability(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	availabilityID, ok := pathID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid availability ID"})
		return
	}
	if err := cc.Service.DeleteAvailability(actorID, availabilityID); err != nil {
		respondError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (cc *Controller) GetProfile(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	result, err := cc.Service.GetProfile(actorID)
	cc.respond(c, result, err)
}

func (cc *Controller) RunMaintenance(c *gin.Context) {
	cc = cc.withContext(c.Request.Context())
	actorID, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}
	if err := cc.Service.RebalanceSchedule(actorID, "manager schedule check"); err != nil {
		respondError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (cc *Controller) respond(c *gin.Context, value interface{}, err error) {
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func respondError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrInvalidStatusChange):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrNoSupportStaff):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, ErrIntegrationNotConfigured), errors.Is(err, ErrMeetingUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func contextUserID(c *gin.Context) (uint, bool) {
	value, ok := c.Get("userID")
	if !ok {
		return 0, false
	}
	switch id := value.(type) {
	case float64:
		if id > 0 {
			return uint(id), true
		}
	case int:
		if id > 0 {
			return uint(id), true
		}
	case uint:
		if id > 0 {
			return id, true
		}
	}
	return 0, false
}

func optionalInt(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, errors.New("invalid integer")
	}
	return value, nil
}
func optionalUint(raw string) (*uint, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		return nil, errors.New("invalid unsigned integer")
	}
	result := uint(value)
	return &result, nil
}
func pathID(c *gin.Context, parameter string) (uint, bool) {
	id, err := strconv.ParseUint(c.Param(parameter), 10, 64)
	return uint(id), err == nil && id > 0
}
func actorAndCall(c *gin.Context) (uint, uint, bool) {
	actor, ok := contextUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return 0, 0, false
	}
	callID, ok := pathID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid call ID"})
		return 0, 0, false
	}
	return actor, callID, true
}
