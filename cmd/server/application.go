package main

import (
	"errors"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"nordik-drive-api/config"
	"nordik-drive-api/internal/admin"
	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/chat"
	"nordik-drive-api/internal/community"
	"nordik-drive-api/internal/dataconfig"
	"nordik-drive-api/internal/file"
	"nordik-drive-api/internal/formsubmission"
	"nordik-drive-api/internal/honour"
	"nordik-drive-api/internal/logocontent"
	"nordik-drive-api/internal/logs"
	"nordik-drive-api/internal/lookup"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/observability"
	"nordik-drive-api/internal/role"
	"nordik-drive-api/internal/supportrequest"
	"nordik-drive-api/internal/supportschedule"
)

type application struct {
	Router  *gin.Engine
	Mailer  *mailer.Service
	Honour  *honour.Service
	Support *supportschedule.Service
	Close   func()
}

func newApplication(cfg config.Config, dependencies *runtimeDependencies) (*application, error) {
	db := dependencies.DB
	r := gin.New()
	r.Use(observability.Middleware())
	r.GET("/healthz", func(c *gin.Context) { c.Status(200) })
	observability.RegisterControls(r, dependencies.Pool)

	r.Use(cors.New(cors.Config{
		AllowOrigins:     config.AllowedOrigins(),
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: true,
		ExposeHeaders:    []string{"X-Request-ID", "Retry-After"},
	}))

	mailerService := mailer.NewService(
		cfg.GmailUser,
		cfg.GmailPass,
		"smtp.gmail.com",
		"587",
	)

	logService := &logs.LogService{DB: db}
	userService := &auth.AuthService{DB: db, CFG: &cfg, Mailer: mailerService}
	auth.RegisterRoutes(r, userService, logService)

	fileService := &file.FileService{DB: db, Mailer: mailerService}
	file.RegisterRoutes(r, fileService, logService)

	roleService := &role.RoleService{DB: db}
	role.RegisterRoutes(r, roleService)

	logs.RegisterRoutes(r, logService)

	communityService := &community.CommunityService{DB: db}
	community.RegisterRoutes(r, communityService)

	configService := &dataconfig.DataConfigService{DB: db}
	dataconfig.RegisterRoutes(r, configService)

	formSubmissionService := &formsubmission.FormSubmissionService{DB: db, Mailer: mailerService}
	formsubmission.RegisterRoutes(r, formSubmissionService)

	supportRequestService := &supportrequest.SupportRequestService{
		DB:                     db,
		Mailer:                 mailerService,
		NotificationRecipients: cfg.SupportRequestNotificationEmails,
	}
	supportrequest.RegisterRoutes(r, supportRequestService)

	var meetingProvider supportschedule.MeetingProvider = supportschedule.DisabledMeetingProvider{}
	if cfg.ZoomEnabled {
		zoomProvider, err := supportschedule.NewZoomMeetingProvider(supportschedule.ZoomConfig{
			AccountID: cfg.ZoomAccountID, ClientID: cfg.ZoomClientID, ClientSecret: cfg.ZoomClientSecret,
		})
		if err != nil {
			return nil, errors.New("invalid Zoom integration configuration")
		}
		meetingProvider = zoomProvider
	}
	supportScheduleService := &supportschedule.Service{DB: db, Mailer: mailerService, MeetingProvider: meetingProvider}
	supportschedule.RegisterRoutes(r, supportScheduleService)

	lookupService := &lookup.LookupService{DB: db}
	lookup.RegisterRoutes(r, lookupService)

	logoContentService := logocontent.NewLogoContentService(db)
	logocontent.RegisterRoutes(r, logoContentService)

	chatService := &chat.ChatService{DB: db, Client: dependencies.Vertex}
	chatBackend, closeChatBackend, err := chat.NewConfiguredChatBackend(chatService)
	if err != nil {
		return nil, errors.New("invalid chat backend configuration")
	}
	chat.RegisterRoutes(r, chatService, chatBackend)

	honourService := honour.NewService(db, chatService)
	appLocation := loadAppLocation(cfg.AppTimeZone)
	honourService.Location = appLocation
	honour.RegisterRoutes(r, honourService)

	adminService := &admin.AdminService{DB: db}
	admin.RegisterRoutes(r, adminService)

	return &application{Router: r, Mailer: mailerService, Honour: honourService, Support: supportScheduleService, Close: closeChatBackend}, nil
}
