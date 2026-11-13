// Package identity owns the users/principals repository and its private API.
package identity

import (
	"context"
	"strings"

	"github.com/lib/pq"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"

	pb "nordik-drive-api/internal/identitypb"
)

type Policy struct {
	Profiles, Principals, Email map[string]bool
}
type callerKey struct{}

type Service struct {
	pb.UnimplementedIdentityServer
	DB     *gorm.DB
	Policy Policy
}

func validIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, status.Error(codes.InvalidArgument, "between one and one hundred IDs required")
	}
	seen := map[int64]bool{}
	result := []int64{}
	for _, id := range ids {
		if id <= 0 {
			return nil, status.Error(codes.InvalidArgument, "IDs must be positive")
		}
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result, nil
}

func allowed(ctx context.Context, accounts map[string]bool) bool {
	account, _ := ctx.Value(callerKey{}).(string)
	return account != "" && accounts[account]
}

func (s *Service) BatchGetProfiles(ctx context.Context, req *pb.BatchGetProfilesRequest) (*pb.BatchGetProfilesResponse, error) {
	if !allowed(ctx, s.Policy.Profiles) || (req.GetIncludeEmail() && !allowed(ctx, s.Policy.Email)) {
		return nil, status.Error(codes.PermissionDenied, "profile access denied")
	}
	ids, err := validIDs(req.GetIds())
	if err != nil {
		return nil, err
	}
	columns := "id,firstname,lastname"
	if req.IncludeEmail {
		columns += ",email"
	}
	var rows []struct {
		ID                         int64
		Firstname, Lastname, Email string
	}
	if err := s.DB.WithContext(ctx).Table("users").Select(columns).Where("id = ANY(?)", pq.Array(ids)).Order("id").Scan(&rows).Error; err != nil {
		return nil, status.Error(codes.Unavailable, "identity repository unavailable")
	}
	out := &pb.BatchGetProfilesResponse{}
	found := map[int64]bool{}
	for _, row := range rows {
		found[row.ID] = true
		out.Profiles = append(out.Profiles, &pb.Profile{Id: row.ID, Firstname: row.Firstname, Lastname: row.Lastname, Email: row.Email})
	}
	for _, id := range ids {
		if !found[id] {
			out.MissingIds = append(out.MissingIds, id)
		}
	}
	return out, nil
}

func (s *Service) BatchGetPrincipals(ctx context.Context, req *pb.BatchGetPrincipalsRequest) (*pb.BatchGetPrincipalsResponse, error) {
	if !allowed(ctx, s.Policy.Principals) {
		return nil, status.Error(codes.PermissionDenied, "principal access denied")
	}
	ids, err := validIDs(req.GetIds())
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID        int64
		Role      string
		Community pq.StringArray `gorm:"type:text[]"`
	}
	if err := s.DB.WithContext(ctx).Table("users").Select("id,role,community").Where("id = ANY(?)", pq.Array(ids)).Order("id").Scan(&rows).Error; err != nil {
		return nil, status.Error(codes.Unavailable, "identity repository unavailable")
	}
	out := &pb.BatchGetPrincipalsResponse{}
	found := map[int64]bool{}
	for _, row := range rows {
		found[row.ID] = true
		out.Principals = append(out.Principals, &pb.Principal{Id: row.ID, Role: row.Role, Communities: []string(row.Community)})
	}
	for _, id := range ids {
		if !found[id] {
			out.MissingIds = append(out.MissingIds, id)
		}
	}
	return out, nil
}

func (s *Service) SearchProfiles(ctx context.Context, req *pb.SearchProfilesRequest) (*pb.SearchProfilesResponse, error) {
	if !allowed(ctx, s.Policy.Profiles) || (req.GetRole() != "" && !allowed(ctx, s.Policy.Principals)) {
		return nil, status.Error(codes.PermissionDenied, "profile search denied")
	}
	if req == nil || len(req.Query) > 1024 || req.AfterId < 0 || req.Limit > 100 || (req.MatchMode != "" && req.MatchMode != "parts" && req.MatchMode != "full") {
		return nil, status.Error(codes.InvalidArgument, "invalid profile search")
	}
	limit := int(req.Limit)
	if limit == 0 {
		limit = 100
	}
	q := s.DB.WithContext(ctx).Table("users").Select("id").Where("id > ?", req.AfterId).Order("id").Limit(limit + 1)
	if req.Query != "" {
		pattern := "%" + strings.TrimSpace(req.Query) + "%"
		if req.MatchMode == "full" {
			q = q.Where("(COALESCE(firstname,'') || ' ' || COALESCE(lastname,'')) ILIKE ?", pattern)
		} else {
			q = q.Where("COALESCE(firstname,'') ILIKE ? OR COALESCE(lastname,'') ILIKE ?", pattern, pattern)
		}
	}
	if req.Role != "" {
		q = q.Where("LOWER(role) = LOWER(?)", req.Role)
	}
	var ids []int64
	if err := q.Pluck("id", &ids).Error; err != nil {
		return nil, status.Error(codes.Unavailable, "identity search unavailable")
	}
	out := &pb.SearchProfilesResponse{Ids: ids}
	if len(ids) > limit {
		out.Ids = ids[:limit]
		out.NextAfterId = out.Ids[limit-1]
	}
	return out, nil
}
