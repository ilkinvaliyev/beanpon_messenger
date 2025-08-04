package utils

import "strings"

const BaseURL = "https://api.beanpon.com"
const StoragePrefix = "/storage/"

func PrependBaseURL(path *string) *string {
	if path == nil || *path == "" {
		return nil
	}

	// Zaten tam URL isə dəyişdirmə
	if strings.HasPrefix(*path, "http://") || strings.HasPrefix(*path, "https://") {
		return path
	}

	cleanedPath := strings.TrimPrefix(*path, "/") // baştaki / işarəsini sil
	fullURL := BaseURL + StoragePrefix + cleanedPath

	return &fullURL
}
