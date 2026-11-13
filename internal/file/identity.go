package file

import (
	"gorm.io/gorm"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/requestscope"
)

func (s *FileService) profileMap(ids []int64, email bool) (map[int64]identityclient.Profile, error) {
	if len(ids) == 0 {
		return map[int64]identityclient.Profile{}, nil
	}
	return s.Directory.Profiles(requestscope.Context(s.DB), ids, email)
}

func (s *FileService) getAllFilesOwned(userID uint, role string) ([]FileWithUser, error) {
	var rows []FileWithUser
	query := s.DB.Table("file f").Select("f.*")
	if role != "Admin" {
		query = query.Joins("LEFT JOIN file_access fa ON f.id=fa.file_id AND fa.user_id=?", userID).Where("(f.private=false OR fa.user_id=?) AND f.is_delete=?", userID, false)
	}
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, row := range rows {
		if row.InsertedBy > 0 {
			ids = append(ids, int64(row.InsertedBy))
		}
	}
	profiles, err := s.profileMap(ids, false)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		p := profiles[int64(rows[i].InsertedBy)]
		rows[i].Firstname = p.Firstname
		rows[i].Lastname = p.Lastname
	}
	return rows, nil
}

func (s *FileService) getFileAccessOwned(id string) ([]FileAccessWithUser, error) {
	var rows []FileAccessWithUser
	if err := s.DB.Table("file_access").Select("id,user_id,file_id").Where("file_id=?", id).Scan(&rows).Error; err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, row := range rows {
		if row.UserID > 0 {
			ids = append(ids, int64(row.UserID))
		}
	}
	profiles, err := s.profileMap(ids, false)
	if err != nil {
		return nil, err
	}
	kept := rows[:0]
	for _, row := range rows {
		if p, found := profiles[int64(row.UserID)]; found {
			row.FirstName = p.Firstname
			row.LastName = p.Lastname
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// Resolve the recipient before starting the mutation/row-lock transaction. The
// immutable request owner is checked again when the notification intent commits.
func (s *FileService) prepareReviewRecipient(id uint) (*FileService, error) {
	var row struct{ UserID uint }
	if err := s.DB.Model(&FileEditRequest{}).Select("user_id").Where("request_id=?", id).First(&row).Error; err != nil {
		return nil, err
	}
	profiles, err := s.profileMap([]int64{int64(row.UserID)}, true)
	if err != nil {
		return nil, err
	}
	if _, found := profiles[int64(row.UserID)]; !found {
		return nil, gorm.ErrRecordNotFound
	}
	copy := *s
	copy.reviewRecipients = profiles
	return &copy, nil
}
