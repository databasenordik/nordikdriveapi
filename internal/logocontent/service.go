package logocontent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"cloud.google.com/go/storage"
	"gorm.io/gorm"

	"nordik-drive-api/internal/requestscope"
)

var newGCSClientHook = func(ctx context.Context) (*storage.Client, error) {
	return storage.NewClient(ctx)
}

type LogoContentServiceAPI interface {
	GetHTMLByFileID(fileID uint) ([]byte, string, string, error)
}

type LogoContentService struct {
	DB *gorm.DB
}

func NewLogoContentService(db *gorm.DB) *LogoContentService {
	return &LogoContentService{DB: db}
}

func (s *LogoContentService) GetHTMLByFileID(fileID uint) ([]byte, string, string, error) {
	var rec FileLogoContent
	if err := s.DB.Where("file_id = ?", fileID).First(&rec).Error; err != nil {
		return nil, "", "", err
	}

	return s.readHTMLFromGCS(rec.LogoURL)
}

func (s *LogoContentService) readHTMLFromGCS(gsURL string) ([]byte, string, string, error) {
	bucket, objectPath, err := parseGSURL(gsURL)
	if err != nil {
		return nil, "", "", err
	}
	if !hasHTMLExtension(objectPath) {
		return nil, "", "", fmt.Errorf("logo url must point to an html file")
	}

	ctx := requestscope.Context(s.DB)
	client, err := newGCSClientHook(ctx)
	if err != nil {
		return nil, "", "", err
	}
	defer client.Close()

	rc, err := client.Bucket(bucket).Object(objectPath).NewReader(ctx)
	if err != nil {
		return nil, "", "", err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", "", err
	}

	contentType, err := normalizeHTMLContentType(rc.ContentType(), data, objectPath)
	if err != nil {
		return nil, "", "", err
	}

	filename := path.Base(objectPath)
	if filename == "." || filename == "/" || filename == "" {
		filename = "logo-content.html"
	}

	return data, contentType, filename, nil
}

func parseGSURL(gsURL string) (bucket string, objectPath string, err error) {
	gsURL = strings.TrimSpace(gsURL)
	if gsURL == "" {
		return "", "", fmt.Errorf("empty gs url")
	}
	if !strings.HasPrefix(gsURL, "gs://") {
		return "", "", fmt.Errorf("invalid gs url (must start with gs://): %s", gsURL)
	}

	rest := strings.TrimPrefix(gsURL, "gs://")
	slash := strings.Index(rest, "/")
	if slash <= 0 || slash == len(rest)-1 {
		return "", "", fmt.Errorf("invalid gs url format: %s", gsURL)
	}

	bucket = strings.TrimSpace(rest[:slash])
	objectPath = strings.TrimSpace(rest[slash+1:])
	if bucket == "" || objectPath == "" {
		return "", "", fmt.Errorf("invalid gs url format: %s", gsURL)
	}

	return bucket, objectPath, nil
}

func hasHTMLExtension(objectPath string) bool {
	ext := strings.ToLower(path.Ext(strings.TrimSpace(objectPath)))
	return ext == ".html" || ext == ".htm"
}

func isHTMLContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(contentType, "text/html") || strings.HasPrefix(contentType, "application/xhtml+xml")
}

func normalizeHTMLContentType(contentType string, data []byte, objectPath string) (string, error) {
	trimmed := strings.TrimSpace(contentType)
	if isHTMLContentType(trimmed) {
		return trimmed, nil
	}

	detected := http.DetectContentType(data)
	if isHTMLContentType(detected) {
		return "text/html; charset=utf-8", nil
	}

	lowerTrimmed := strings.ToLower(trimmed)
	if hasHTMLExtension(objectPath) && (lowerTrimmed == "" || strings.HasPrefix(lowerTrimmed, "application/octet-stream") || strings.HasPrefix(lowerTrimmed, "text/plain")) {
		return "text/html; charset=utf-8", nil
	}

	return "", fmt.Errorf("object is not html content")
}
