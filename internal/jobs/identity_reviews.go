package jobs

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/requestscope"
)

// Preserve the old inner-join batch semantics: missing profiles must not occupy
// every scan slot and starve valid recipients. Select all owner profiles before
// entering any business transaction, then give recovery an immutable snapshot.
func ownedReviewRows[T any](query *gorm.DB, columns, createdColumn, idColumn string, limit int, directory identityclient.Directory, actor func(T) int64, key func(T) (*time.Time, int64)) ([]T, identityclient.Directory, map[int64]identityclient.Profile, error) {
	ctx, stop := context.WithTimeout(requestscope.Context(query), 20*time.Second)
	defer stop()
	if limit <= 0 {
		limit = 100
	}
	pageSize := min(limit, 100)
	selected := []T{}
	selectedIDs := []int64{}
	profiles := map[int64]identityclient.Profile{}
	finish := func() ([]T, identityclient.Directory, map[int64]identityclient.Profile, error) {
		snapshot, err := identityclient.NewProfileSnapshot(selectedIDs, profiles, true)
		return selected, snapshot, profiles, err
	}
	var after *time.Time
	var afterID int64
	for page := 0; page < 100; page++ {
		q := query.WithContext(ctx).Session(&gorm.Session{}).Select(columns)
		if page > 0 {
			if after == nil {
				q = q.Where(createdColumn+" IS NULL AND "+idColumn+" > ?", afterID)
			} else {
				q = q.Where("("+createdColumn+" > ? OR ("+createdColumn+" = ? AND "+idColumn+" > ?) OR "+createdColumn+" IS NULL)", *after, *after, afterID)
			}
		}
		var candidates []T
		if err := q.Order(createdColumn + " ASC NULLS LAST").Order(idColumn + " ASC").Limit(pageSize).Scan(&candidates).Error; err != nil {
			return nil, nil, nil, err
		}
		if len(candidates) == 0 {
			return finish()
		}
		ids := []int64{}
		for _, row := range candidates {
			if id := actor(row); id > 0 {
				ids = append(ids, id)
			}
		}
		found, err := directory.Profiles(ctx, ids, true)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, row := range candidates {
			if p, exists := found[actor(row)]; exists {
				selected = append(selected, row)
				selectedIDs = append(selectedIDs, p.ID)
				profiles[p.ID] = p
				if len(selected) >= limit {
					return finish()
				}
			}
		}
		if len(candidates) < pageSize {
			return finish()
		}
		after, afterID = key(candidates[len(candidates)-1])
	}
	return nil, nil, nil, errors.New("review recipient scan budget exhausted; no business changes committed")
}
