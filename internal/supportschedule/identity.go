package supportschedule

import (
	"errors"
	"strings"

	"gorm.io/gorm"
	"nordik-drive-api/internal/requestscope"
)

type supportIdentitySnapshot struct {
	known map[uint]bool
	users map[uint]SupportUser
	roles map[string][]uint
}

type userScope struct {
	Extra, Requests, Calls []uint
	Dates                  []string
	Availability           uint
}
type userTarget struct {
	id  uint
	set func(*SupportUser)
}

func inBusinessTransaction(db *gorm.DB) bool {
	if db == nil || db.Statement == nil {
		return false
	}
	_, ok := db.Statement.ConnPool.(gorm.TxCommitter)
	return ok
}

func (s *Service) identityUsers(db *gorm.DB, ids []uint) (map[uint]SupportUser, error) {
	result := map[uint]SupportUser{}
	if s.identitySnapshot != nil {
		for _, id := range ids {
			if id == 0 {
				continue
			}
			if !s.identitySnapshot.known[id] {
				return nil, errors.New("support identity must be prepared before transaction")
			}
			if user, found := s.identitySnapshot.users[id]; found {
				result[id] = user
			}
		}
		return result, nil
	}
	keys := []int64{}
	for _, id := range ids {
		if id > 0 {
			keys = append(keys, int64(id))
		}
	}
	if len(keys) == 0 {
		return result, nil
	}
	if inBusinessTransaction(db) {
		return nil, errors.New("support identity must be prepared before transaction")
	}
	profiles, err := s.Directory.Profiles(requestscope.Context(db), keys, true)
	if err != nil {
		return nil, err
	}
	principals, err := s.Directory.Principals(requestscope.Context(db), keys)
	if err != nil {
		return nil, err
	}
	for id, p := range profiles {
		if principal, found := principals[id]; found {
			result[uint(id)] = SupportUser{ID: uint(id), FirstName: p.Firstname, LastName: p.Lastname, Email: p.Email, Role: principal.Role}
		}
	}
	return result, nil
}

func (s *Service) roleUsers(db *gorm.DB, role string) ([]SupportUser, error) {
	if s.Directory == nil {
		var users []SupportUser
		err := db.Where("LOWER(role) = ?", strings.ToLower(role)).Find(&users).Error
		return users, err
	}
	var ids []uint
	if s.identitySnapshot != nil {
		var found bool
		ids, found = s.identitySnapshot.roles[strings.ToLower(role)]
		if !found {
			return nil, errors.New("support role selection was not prepared before transaction")
		}
	} else {
		if inBusinessTransaction(db) {
			return nil, errors.New("support role selection must precede transaction")
		}
		keys, err := s.Directory.Search(requestscope.Context(db), "", "parts", role)
		if err != nil {
			return nil, err
		}
		for _, id := range keys {
			ids = append(ids, uint(id))
		}
	}
	users, err := s.identityUsers(db, ids)
	if err != nil {
		return nil, err
	}
	result := []SupportUser{}
	for _, id := range ids {
		if user, found := users[id]; found && strings.EqualFold(user.Role, role) {
			result = append(result, user)
		}
	}
	return result, nil
}

// Only business-owned reference IDs and eligible recipient roles are collected.
// Both owner APIs complete before any subsequent mutation/row-lock transaction.
func (s *Service) prepareIdentity(scope userScope) (*Service, error) {
	if s.Directory == nil || s.identitySnapshot != nil {
		return s, nil
	}
	if inBusinessTransaction(s.DB) {
		return nil, errors.New("support identity preparation cannot run inside transaction")
	}
	ids := append([]uint{}, scope.Extra...)
	roles := map[string][]uint{}
	for _, role := range []string{RoleAdmin, RoleManager} {
		keys, err := s.Directory.Search(requestscope.Context(s.DB), "", "parts", role)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			id := uint(key)
			ids = append(ids, id)
			roles[strings.ToLower(role)] = append(roles[strings.ToLower(role)], id)
		}
		if roles[strings.ToLower(role)] == nil {
			roles[strings.ToLower(role)] = []uint{}
		}
	}
	var members []uint
	if err := s.DB.Model(&SupportTeamMember{}).Pluck("user_id", &members).Error; err != nil {
		return nil, err
	}
	ids = append(ids, members...)
	if scope.Availability > 0 {
		var row StaffAvailability
		if err := s.DB.First(&row, scope.Availability).Error; err != nil {
			return nil, mapNotFound(err)
		}
		scope.Dates = append(scope.Dates, row.AvailabilityDate)
		ids = append(ids, row.StaffID)
	}
	if len(scope.Calls) > 0 {
		var calls []SupportCall
		if err := s.DB.Where("id IN ?", scope.Calls).Find(&calls).Error; err != nil {
			return nil, err
		}
		for _, row := range calls {
			scope.Requests = append(scope.Requests, row.SupportRequestID)
			if row.AssignedStaffID != nil {
				ids = append(ids, *row.AssignedStaffID)
			}
		}
	}
	if len(scope.Requests) > 0 || len(scope.Dates) > 0 {
		var requests []SupportCallRequest
		if err := s.DB.Where("id IN ? OR requested_date IN ?", scope.Requests, scope.Dates).Find(&requests).Error; err != nil {
			return nil, err
		}
		for _, row := range requests {
			ids = append(ids, row.RequestedByUserID)
			if row.RequestedStaffID != nil {
				ids = append(ids, *row.RequestedStaffID)
			}
			if row.AssignedStaffID != nil {
				ids = append(ids, *row.AssignedStaffID)
			}
		}
	}
	if len(scope.Dates) > 0 {
		var assignments []SupportAssignment
		if err := s.DB.Where("assignment_date IN ?", scope.Dates).Find(&assignments).Error; err != nil {
			return nil, err
		}
		for _, row := range assignments {
			if row.PrimaryAssigneeID != nil {
				ids = append(ids, *row.PrimaryAssigneeID)
			}
			if row.PreviousAssigneeID != nil {
				ids = append(ids, *row.PreviousAssigneeID)
			}
		}
	}
	users, err := s.identityUsers(s.DB, ids)
	if err != nil {
		return nil, err
	}
	known := map[uint]bool{}
	for _, id := range ids {
		if id > 0 {
			known[id] = true
		}
	}
	copy := *s
	copy.identitySnapshot = &supportIdentitySnapshot{known: known, users: users, roles: roles}
	return &copy, nil
}

func (s *Service) userPreload(name string) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if s.Directory != nil {
			return db
		}
		return db.Preload(name)
	}
}

func (s *Service) hydrate(db *gorm.DB, targets []userTarget) error {
	if s.Directory == nil {
		return nil
	}
	ids := []uint{}
	for _, target := range targets {
		if target.id > 0 {
			ids = append(ids, target.id)
		}
	}
	users, err := s.identityUsers(db, ids)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if user, found := users[target.id]; found {
			copy := user
			target.set(&copy)
		} else {
			target.set(nil)
		}
	}
	return nil
}

func assignmentTargets(rows ...*SupportAssignment) []userTarget {
	result := []userTarget{}
	for _, row := range rows {
		if row.PrimaryAssigneeID != nil {
			result = append(result, userTarget{*row.PrimaryAssigneeID, func(user *SupportUser) { row.PrimaryAssignee = user }})
		}
		if row.PreviousAssigneeID != nil {
			result = append(result, userTarget{*row.PreviousAssigneeID, func(user *SupportUser) { row.PreviousAssignee = user }})
		}
	}
	return result
}
func assignmentsTargets(rows []SupportAssignment) []userTarget {
	refs := make([]*SupportAssignment, len(rows))
	for i := range rows {
		refs[i] = &rows[i]
	}
	return assignmentTargets(refs...)
}
func memberTargets(rows []SupportTeamMember) []userTarget {
	result := []userTarget{}
	for i := range rows {
		row := &rows[i]
		result = append(result, userTarget{row.UserID, func(user *SupportUser) {
			row.User = SupportUser{}
			if user != nil {
				row.User = *user
			}
		}})
	}
	return result
}
func availabilityTargets(rows []StaffAvailability) []userTarget {
	result := []userTarget{}
	for i := range rows {
		row := &rows[i]
		result = append(result, userTarget{row.StaffID, func(user *SupportUser) { row.Staff = user }})
	}
	return result
}
func callTargets(rows []SupportCall) []userTarget {
	result := []userTarget{}
	for i := range rows {
		row := &rows[i]
		if row.AssignedStaffID != nil {
			result = append(result, userTarget{*row.AssignedStaffID, func(user *SupportUser) { row.AssignedStaff = user }})
		}
	}
	return result
}

func callRefTargets(rows ...*SupportCall) []userTarget {
	result := []userTarget{}
	for _, row := range rows {
		if row.AssignedStaffID != nil {
			result = append(result, userTarget{*row.AssignedStaffID, func(user *SupportUser) { row.AssignedStaff = user }})
		}
	}
	return result
}
func primaryAssignmentTargets(rows ...*SupportAssignment) []userTarget {
	result := []userTarget{}
	for _, row := range rows {
		if row.PrimaryAssigneeID != nil {
			result = append(result, userTarget{*row.PrimaryAssigneeID, func(user *SupportUser) { row.PrimaryAssignee = user }})
		}
	}
	return result
}

func primaryAssignmentsTargets(rows []SupportAssignment) []userTarget {
	refs := make([]*SupportAssignment, len(rows))
	for i := range rows {
		refs[i] = &rows[i]
	}
	return primaryAssignmentTargets(refs...)
}
func requestTargets(rows ...*SupportCallRequest) []userTarget {
	result := []userTarget{}
	for _, row := range rows {
		result = append(result, userTarget{row.RequestedByUserID, func(user *SupportUser) {
			row.RequestedBy = SupportUser{}
			if user != nil {
				row.RequestedBy = *user
			}
		}})
		if row.RequestedStaffID != nil {
			result = append(result, userTarget{*row.RequestedStaffID, func(user *SupportUser) { row.RequestedStaff = user }})
		}
		if row.AssignedStaffID != nil {
			result = append(result, userTarget{*row.AssignedStaffID, func(user *SupportUser) { row.AssignedStaff = user }})
		}
	}
	return result
}
func requestsTargets(rows []SupportCallRequest) []userTarget {
	refs := make([]*SupportCallRequest, len(rows))
	for i := range rows {
		refs[i] = &rows[i]
	}
	return requestTargets(refs...)
}
