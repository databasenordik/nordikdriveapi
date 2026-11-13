package identityclient

import (
	"context"
	"errors"
)

// SnapshotProfiles resolves a bounded job/request batch before any business
// transaction. Unknown IDs cannot trigger a hidden remote call inside a lock.
func SnapshotProfiles(ctx context.Context, directory Directory, ids []int64, email bool) (Directory, error) {
	if directory == nil {
		return nil, errors.New("identity directory is required")
	}
	profiles, err := directory.Profiles(ctx, ids, email)
	if err != nil {
		return nil, err
	}
	return NewProfileSnapshot(ids, profiles, email)
}

// NewProfileSnapshot freezes profiles already obtained from a validated owner
// response. This avoids another network request after a paginated job selection.
func NewProfileSnapshot(ids []int64, profiles map[int64]Profile, email bool) (Directory, error) {
	if _, err := batches(ids); err != nil {
		return nil, err
	}
	known := map[int64]bool{}
	for _, id := range ids {
		known[id] = true
	}
	copy := map[int64]Profile{}
	for id, profile := range profiles {
		if !known[id] || profile.ID != id {
			return nil, errors.New("profile snapshot contains an unexpected identity")
		}
		if !email {
			profile.Email = ""
		}
		copy[id] = profile
	}
	return &profileSnapshot{known: known, profiles: copy, email: email}, nil
}

type profileSnapshot struct {
	known    map[int64]bool
	profiles map[int64]Profile
	email    bool
}

func (s *profileSnapshot) Profiles(ctx context.Context, ids []int64, email bool) (map[int64]Profile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if email && !s.email {
		return nil, errors.New("recipient email was not prepared before transaction")
	}
	result := map[int64]Profile{}
	for _, id := range ids {
		if !s.known[id] {
			return nil, errors.New("profile was not prepared before transaction")
		}
		if profile, found := s.profiles[id]; found {
			if !email {
				profile.Email = ""
			}
			result[id] = profile
		}
	}
	return result, nil
}
func (s *profileSnapshot) Principals(context.Context, []int64) (map[int64]Principal, error) {
	return nil, errors.New("principals were not prepared before transaction")
}
func (s *profileSnapshot) Search(context.Context, string, string, string) ([]int64, error) {
	return nil, errors.New("search was not prepared before transaction")
}
