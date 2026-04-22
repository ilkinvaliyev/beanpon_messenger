package utils

import (
	"beanpon_messenger/database"
	"regexp"
	"strings"
)

type MentionResponse struct {
	UserID   uint   `json:"user_id"`
	Username string `json:"username"`
	StartPos int    `json:"start_pos"`
	EndPos   int    `json:"end_pos"`
}

func ParseMentions(text string) []MentionResponse {
	re := regexp.MustCompile(`@([\w][\w.]*[\w])`)
	matches := re.FindAllStringSubmatchIndex(text, -1)

	if len(matches) == 0 {
		return []MentionResponse{}
	}

	usernames := make([]string, 0)
	for _, match := range matches {
		usernames = append(usernames, strings.ToLower(text[match[2]:match[3]]))
	}

	var users []struct {
		ID       uint   `gorm:"column:id"`
		Username string `gorm:"column:username"`
	}
	database.DB.Table("users").
		Select("id, username").
		Where("LOWER(username) IN ?", usernames).
		Where("deleted_at IS NULL").
		Find(&users)

	userMap := make(map[string]struct {
		ID       uint
		Username string
	})
	for _, u := range users {
		userMap[strings.ToLower(u.Username)] = struct {
			ID       uint
			Username string
		}{u.ID, u.Username}
	}

	mentions := make([]MentionResponse, 0)
	seen := make(map[string]bool)
	for _, match := range matches {
		username := strings.ToLower(text[match[2]:match[3]])
		if seen[username] {
			continue
		}
		seen[username] = true
		if u, exists := userMap[username]; exists {
			mentions = append(mentions, MentionResponse{
				UserID:   u.ID,
				Username: u.Username,
				StartPos: match[0],
				EndPos:   match[1],
			})
		}
	}

	return mentions
}
