package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// ValidateServer checks required settings separately from environment loading.
// This lets maintenance tools load only the configuration they need.
func (cfg Config) ValidateServer() error {
	if cfg.RuntimeRole != "worker" && strings.TrimSpace(cfg.JWTSecret) == "" {
		return errors.New("JWT_SECRET is required")
	}
	return nil
}

type Config struct {
	RuntimeRole                      string
	VertexProject                    string
	VertexLocation                   string
	DBHost                           string
	DBPort                           string
	DBUser                           string
	DBPassword                       string
	DBName                           string
	DBSchema                         string
	DBAuthMode                       string
	CloudSQLInstance                 string
	CloudSQLIPType                   string
	DBMaxOpen                        int
	JWTSecret                        string
	GeminiKey                        string
	SMTPKey                          string
	GmailUser                        string
	GmailPass                        string
	AppTimeZone                      string
	HonourJobSecret                  string
	SupportRequestNotificationEmails []string
	ZoomEnabled                      bool
	ZoomAccountID                    string
	ZoomClientID                     string
	ZoomClientSecret                 string
}

func LoadConfig() Config {
	appTimeZone := os.Getenv("APP_TIMEZONE")
	if appTimeZone == "" {
		appTimeZone = "America/Toronto"
	}

	supportEmails := splitCommaSeparatedEnv(os.Getenv("SUPPORT_REQUEST_NOTIFICATION_EMAILS"))
	if len(supportEmails) == 0 {
		gmailUser := strings.TrimSpace(os.Getenv("GMAIL_USER"))
		if gmailUser != "" {
			supportEmails = []string{gmailUser}
		}
	}

	return Config{
		RuntimeRole:                      envDefault("APP_ROLE", "monolith"),
		VertexProject:                    envDefault("GOOGLE_CLOUD_PROJECT", "planar-ray-472112-e8"),
		VertexLocation:                   envDefault("VERTEX_LOCATION", "global"),
		DBHost:                           os.Getenv("DB_HOST"),
		DBPort:                           os.Getenv("DB_PORT"),
		DBUser:                           os.Getenv("DB_USER"),
		DBPassword:                       os.Getenv("DB_PASSWORD"),
		DBName:                           os.Getenv("DB_NAME"),
		DBSchema:                         envDefault("DB_SCHEMA", "public"),
		DBAuthMode:                       envDefault("DB_AUTH_MODE", "password"),
		CloudSQLInstance:                 strings.TrimSpace(os.Getenv("CLOUD_SQL_INSTANCE")),
		CloudSQLIPType:                   envDefault("CLOUD_SQL_IP_TYPE", "public"),
		DBMaxOpen:                        positiveIntEnv("DB_MAX_OPEN", 25),
		JWTSecret:                        os.Getenv("JWT_SECRET"),
		GeminiKey:                        os.Getenv("GEMINI_KEY"),
		GmailUser:                        os.Getenv("GMAIL_USER"),
		GmailPass:                        os.Getenv("GMAIL_APP_PASSWORD"),
		AppTimeZone:                      appTimeZone,
		HonourJobSecret:                  os.Getenv("HONOUR_JOB_SECRET"),
		SupportRequestNotificationEmails: supportEmails,
		ZoomEnabled:                      boolEnv("ZOOM_ENABLED"),
		ZoomAccountID:                    strings.TrimSpace(os.Getenv("ZOOM_ACCOUNT_ID")),
		ZoomClientID:                     strings.TrimSpace(os.Getenv("ZOOM_CLIENT_ID")),
		ZoomClientSecret:                 strings.TrimSpace(os.Getenv("ZOOM_CLIENT_SECRET")),
	}
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
func positiveIntEnv(name string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func boolEnv(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}

func splitCommaSeparatedEnv(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))

	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}

		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}

	if len(result) == 0 {
		return nil
	}

	return result
}
