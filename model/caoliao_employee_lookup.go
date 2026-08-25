package model

import "strings"

func GetCaoliaoEmployeeIDsByUserIDs(userIDs []int) (map[int]string, error) {
	result := make(map[int]string)
	if len(userIDs) == 0 {
		return result, nil
	}
	var users []struct {
		Id     int
		Remark string
	}
	err := DB.Model(&User{}).
		Select("id", "remark").
		Where("id IN ?", userIDs).
		Where("remark LIKE ?", caoliaoEmployeePrefix+"%").
		Find(&users).Error
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		result[user.Id] = strings.TrimPrefix(user.Remark, caoliaoEmployeePrefix)
	}
	return result, nil
}
