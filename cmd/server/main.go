package main

import (
	"context"
	"errors"
	"log"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/admin"
	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/chat"
	"nordik-drive-api/internal/community"
	"nordik-drive-api/internal/dataconfig"
	"nordik-drive-api/internal/file"
	"nordik-drive-api/internal/formsubmission"
	"nordik-drive-api/internal/honour"
	"nordik-drive-api/internal/jobs"
	"nordik-drive-api/internal/logocontent"
	"nordik-drive-api/internal/logs"
	"nordik-drive-api/internal/lookup"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/role"
	"nordik-drive-api/internal/supportrequest"
	"nordik-drive-api/internal/supportschedule"
	"os"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"google.golang.org/genai"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg := config.LoadConfig()
	dependencies, err := initializeRuntime(context.Background(), cfg, openDatabase, genai.NewClient)
	if err != nil {
		return err
	}
	defer dependencies.Pool.Close()
	db := dependencies.DB
	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{
			"http://localhost:3000",
			"http://34.145.18.109/",
			"https://nordik-drive-react-724838782318.us-west1.run.app",
			"https://nordik-drive-react-prod-724838782318.us-west1.run.app",
		},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: true,
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
			return errors.New("invalid Zoom integration configuration")
		}
		meetingProvider = zoomProvider
	}
	supportScheduleService := &supportschedule.Service{DB: db, Mailer: mailerService, MeetingProvider: meetingProvider}
	if err := supportScheduleService.RunScheduledMaintenance(); err != nil {
		log.Printf("initial support schedule maintenance failed: %v", err)
	}
	supportschedule.RegisterRoutes(r, supportScheduleService)

	lookupService := &lookup.LookupService{DB: db}
	lookup.RegisterRoutes(r, lookupService)

	logoContentService := logocontent.NewLogoContentService(db)
	logocontent.RegisterRoutes(r, logoContentService)

	chatService := &chat.ChatService{DB: db, Client: dependencies.Vertex}
	chatBackend, closeChatBackend, err := chat.NewConfiguredChatBackend(chatService)
	if err != nil {
		return errors.New("invalid chat backend configuration")
	}
	defer closeChatBackend()
	chat.RegisterRoutes(r, chatService, chatBackend)

	honourService := honour.NewService(db, chatService)
	appLocation := loadAppLocation(cfg.AppTimeZone)
	honourService.Location = appLocation
	honour.RegisterRoutes(r, honourService)

	adminService := &admin.AdminService{DB: db}
	admin.RegisterRoutes(r, adminService)

	// --- Cloud Run expects plain HTTP, on $PORT, bind to 0.0.0.0 ---
	scheduler, err := jobs.NewScheduler(db, mailerService, honourService, supportScheduleService, appLocation, log.Default())
	if err != nil {
		return errors.New("failed to create scheduler")
	}

	scheduler.Start()
	defer scheduler.Stop()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Starting server on 0.0.0.0:%s ...", port)
	return r.Run("0.0.0.0:" + port)
}

func loadAppLocation(name string) *time.Location {
	if name == "" {
		name = "America/Toronto"
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Printf("invalid APP_TIMEZONE=%q, defaulting to America/Toronto: %v", name, err)
		fallback, fallbackErr := time.LoadLocation("America/Toronto")
		if fallbackErr != nil {
			log.Printf("failed to load fallback timezone America/Toronto: %v", fallbackErr)
			return time.Local
		}
		return fallback
	}

	return loc
}
