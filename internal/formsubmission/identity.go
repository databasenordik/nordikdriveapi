package formsubmission

import (
	"errors"
	"gorm.io/gorm"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/requestscope"
	"strings"
)

// Prepare the response identities as well as the recipient before a mutation.
// The transaction then builds its response using only the immutable snapshot;
// an Identity failure cannot turn an already committed update into an HTTP 500.
func (s *FormSubmissionService) prepareMutationProfiles(sub FormSubmission, extra ...int64) (*FormSubmissionService, error) {
	if s.Directory == nil {
		return s, nil
	}
	ids := append([]int64(nil), extra...)
	for _, id := range []*int{sub.CreatedByID, sub.EditedByID, sub.ReviewedByID} {
		if id != nil && *id > 0 {
			ids = append(ids, int64(*id))
		}
	}
	if sub.ID > 0 {
		var uploads []FormSubmissionUpload
		if err := s.DB.Select("reviewed_by").Where("submission_id=?", sub.ID).Find(&uploads).Error; err != nil {
			return nil, err
		}
		for _, row := range uploads {
			if row.ReviewedByID != nil && *row.ReviewedByID > 0 {
				ids = append(ids, int64(*row.ReviewedByID))
			}
		}
	}
	kept := ids[:0]
	for _, id := range ids {
		if id > 0 {
			kept = append(kept, id)
		}
	}
	directory, err := identityclient.SnapshotProfiles(requestscope.Context(s.DB), s.Directory, kept, true)
	if err != nil {
		return nil, err
	}
	copy := *s
	copy.Directory = directory
	return &copy, nil
}
func (s *FormSubmissionService) prepareUpsertProfiles(req *SaveFormSubmissionRequest, actor int) (*FormSubmissionService, error) {
	var sub FormSubmission
	err := s.DB.Select("id,created_by,edited_by,reviewed_by").Where("file_id=? AND row_id=? AND form_key=? AND status<>?", req.FileID, req.RowID, strings.TrimSpace(req.FormKey), ReviewStatusRejected).Order("id desc").First(&sub).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return s.prepareMutationProfiles(sub, int64(actor))
}

func (s *FormSubmissionService) userPreload(name string, args ...any) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if s.Directory != nil {
			return db
		}
		return db.Preload(name, args...)
	}
}

func (s *FormSubmissionService) enrichItems(items []FormSubmission) error {
	refs := make([]*FormSubmission, len(items))
	for i := range items {
		refs[i] = &items[i]
	}
	return s.enrichUsers(refs, nil)
}

func (s *FormSubmissionService) enrichUsers(submissions []*FormSubmission, uploads []FormSubmissionUpload) error {
	if s.Directory == nil {
		return nil
	}
	ids := []int64{}
	add := func(id *int) {
		if id != nil && *id > 0 {
			ids = append(ids, int64(*id))
		}
	}
	for _, row := range submissions {
		add(row.CreatedByID)
		add(row.EditedByID)
		add(row.ReviewedByID)
	}
	for _, row := range uploads {
		add(row.ReviewedByID)
	}
	if len(ids) == 0 {
		return nil
	}
	profiles, err := s.Directory.Profiles(requestscope.Context(s.DB), ids, true)
	if err != nil {
		return err
	}
	lookup := func(id *int) *FormSubmissionUserRef {
		if id == nil {
			return nil
		}
		p, found := profiles[int64(*id)]
		if !found {
			return nil
		}
		return &FormSubmissionUserRef{ID: int(p.ID), FirstName: p.Firstname, LastName: p.Lastname, Email: p.Email}
	}
	for _, row := range submissions {
		row.CreatedByUser = lookup(row.CreatedByID)
		row.EditedByUser = lookup(row.EditedByID)
		row.ReviewedByUser = lookup(row.ReviewedByID)
	}
	for i := range uploads {
		uploads[i].ReviewedByUser = lookup(uploads[i].ReviewedByID)
	}
	return nil
}

func (s *FormSubmissionService) prepareReviewRecipient(id int64) (*FormSubmissionService, error) {
	var row struct{ CreatedBy *int }
	if err := s.DB.Model(&FormSubmission{}).Select("created_by").Where("id=?", id).First(&row).Error; err != nil {
		return nil, err
	}
	if row.CreatedBy == nil || *row.CreatedBy <= 0 {
		return nil, gorm.ErrRecordNotFound
	}
	profiles, err := s.Directory.Profiles(requestscope.Context(s.DB), []int64{int64(*row.CreatedBy)}, true)
	if err != nil {
		return nil, err
	}
	if _, found := profiles[int64(*row.CreatedBy)]; !found {
		return nil, gorm.ErrRecordNotFound
	}
	copy := *s
	copy.reviewRecipients = profiles
	return &copy, nil
}

func (s *FormSubmissionService) preparedCreator(id *int) *FormSubmissionUserRef {
	if id == nil {
		return nil
	}
	p, found := s.reviewRecipients[int64(*id)]
	if !found {
		return nil
	}
	return &FormSubmissionUserRef{ID: int(p.ID), FirstName: p.Firstname, LastName: p.Lastname, Email: p.Email}
}
