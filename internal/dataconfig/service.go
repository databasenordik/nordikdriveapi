package dataconfig

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

type DataConfigService struct {
	DB *gorm.DB
}

type GetConfigResult struct {
	NotModified bool
	Config      *DataConfig
}

// GetByFileNameIfModified:
// - Finds active config by file_name (case-insensitive), latest updated_at.
// - Always loads the current content; cache validation is content based.
func (s *DataConfigService) GetByFileNameIfModified(fileName string, clientLastModified *time.Time) (*GetConfigResult, error) {
	name := strings.TrimSpace(fileName)
	if name == "" {
		return nil, errors.New("file_name is required")
	}

	var cfg DataConfig
	err := s.DB.
		Where("is_active = ?", true).
		Where("lower(file_name) = lower(?)", name).
		Order("updated_at DESC").
		Order("id DESC").
		Take(&cfg).Error

	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, gorm.ErrRecordNotFound
		}
		return nil, err
	}

	// Direct JSON edits may leave updated_at unchanged. A timestamp alone cannot
	// establish that the client has the current configuration. The controller
	// compares a checksum computed from the returned content instead.

	return &GetConfigResult{NotModified: false, Config: &cfg}, nil
}
