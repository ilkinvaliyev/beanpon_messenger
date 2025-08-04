package utils

import "strings"

const BaseURL = "https://api.beanpon.com/"

// PrependBaseURL resim yoluna domain prefix-ləyir
func PrependBaseURL(path *string) *string {
	if path == nil || *path == "" {
		return nil
	}

	// Zaten tam URL'se, aynen dön
	if strings.HasPrefix(*path, "http://") || strings.HasPrefix(*path, "https://") {
		return path
	}

	fullURL := BaseURL + strings.TrimPrefix(*path, "/")
	return &fullURL
}
