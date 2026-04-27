package utils

import (
	"beanpon_messenger/config"
	"strings"
)

func ContainsBadWord(text string) bool {
	lower := strings.ToLower(text)
	for _, word := range config.BlockedWords {
		if strings.Contains(lower, strings.ToLower(word)) {
			return true
		}
	}
	return false
}
