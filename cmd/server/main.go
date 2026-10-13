package main

import (
	"context"
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
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	cfg := config.LoadConfig()

	dsn := "host=" + cfg.DBHost +
		" user=" + cfg.DBUser +
		" password=" + cfg.DBPassword +
		" dbname=" + cfg.DBName +
		" port=" + cfg.DBPort +
		" sslmode=disable"

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{PrepareStmt: true})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal("Failed to configure database pool:", err)
	}
	// Bound connection growth per Cloud Run instance while retaining enough warm
	// connections for concurrent chat metadata and aggregate queries.
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
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
			log.Fatalf("invalid Zoom integration configuration: %v", err)
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

	// Create client with ADC (production)
	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  "planar-ray-472112-e8", // <-- REPLACE with your Project ID
		Location: "global",               // <-- REPLACE with your project location
		// Note: No APIKey is needed when using Vertex AI with ADC.
	})

	chatService := &chat.ChatService{DB: db, Client: client}
	chatBackend, closeChatBackend, err := chat.NewConfiguredChatBackend(chatService)
	if err != nil {
		log.Fatalf("invalid chat backend configuration: %v", err)
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
		log.Fatalf("failed to create scheduler: %v", err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Starting server on 0.0.0.0:%s ...", port)
	log.Fatal(r.Run("0.0.0.0:" + port))
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
