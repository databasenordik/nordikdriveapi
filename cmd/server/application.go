package main

import (
	"errors"
	"os"
	"strconv"

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
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/requestscope"
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
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "Idempotency-Key"},
		AllowCredentials: true,
		ExposeHeaders:    []string{"X-Request-ID", "Retry-After"},
	}))

	writer, remote, err := outbox.Configure(requestscope.Context(db))
	if err != nil {
		return nil, err
	}
	var mailerService *mailer.Service
	if writer == nil {
		mailerService = mailer.NewService(
			cfg.GmailUser,
			cfg.GmailPass,
			"smtp.gmail.com",
			"587",
		)
	}

	logService := &logs.LogService{DB: db}
	userService := &auth.AuthService{DB: db, CFG: &cfg, Mailer: mailerService}
	userService.Outbox = writer
	if flag := os.Getenv("SIGNUP_REQUIRE_VERIFIED_EMAIL"); flag != "" {
		userService.RequireVerified, err = strconv.ParseBool(flag)
		if err != nil {
			return nil, errors.New("SIGNUP_REQUIRE_VERIFIED_EMAIL must be a boolean")
		}
	}
	if writer != nil && os.Getenv("SIGNUP_VERIFICATION_KEYS") != "" {
		userService.Signup, err = auth.NewSignupVerifier(db, writer, os.Getenv("SIGNUP_VERIFICATION_KEYS"), os.Getenv("SIGNUP_VERIFICATION_KEY_VERSION"))
		if err != nil {
			return nil, err
		}
		userService.Signup.Remote = remote
	}
	if userService.RequireVerified && userService.Signup == nil {
		return nil, errors.New("mandatory signup verification is not configured")
	}
	auth.RegisterRoutes(r, userService, logService)

	fileService := &file.FileService{DB: db, Mailer: mailerService}
	fileService.Outbox = writer
	file.RegisterRoutes(r, fileService, logService)

	roleService := &role.RoleService{DB: db}
	role.RegisterRoutes(r, roleService)

	logs.RegisterRoutes(r, logService)

	communityService := &community.CommunityService{DB: db}
	community.RegisterRoutes(r, communityService)

	configService := &dataconfig.DataConfigService{DB: db}
	dataconfig.RegisterRoutes(r, configService)

	formSubmissionService := &formsubmission.FormSubmissionService{DB: db, Mailer: mailerService}
	formSubmissionService.Outbox = writer
	formsubmission.RegisterRoutes(r, formSubmissionService)

	supportRequestService := &supportrequest.SupportRequestService{
		DB:                     db,
		Mailer:                 mailerService,
		NotificationRecipients: cfg.SupportRequestNotificationEmails,
	}
	supportrequest.RegisterRoutes(r, supportRequestService)
	supportRequestService.Outbox = writer

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
	supportScheduleService.Outbox = writer
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
