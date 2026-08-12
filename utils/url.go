package utils

import "strings"

// BaseURL — media full URL prefiksi. Default api.beanpon.com; main.go
// SetBaseURL(cfg.AppURL) ilə override edir (Laravel APP_URL ilə byte-uyğunluq).
var BaseURL = "https://api.beanpon.com"

// SetBaseURL — APP_URL-i tətbiq başlanğıcında təyin edir.
func SetBaseURL(u string) {
	if u != "" {
		BaseURL = strings.TrimRight(u, "/")
	}
}

// FilePathS3 — Laravel filePathS3($key): BaseURL/api/s3-storage/key.
// Səs/media upload cavabında tam URL qaytarmaq üçün (nil-safe).
func FilePathS3(key string) string {
	if key == "" {
		return ""
	}
	return BaseURL + "/api/s3-storage/" + strings.TrimPrefix(key, "/")
}

type StorageType string

const (
	StorageLocal StorageType = "storage"
	StorageS3    StorageType = "api/s3-storage"
)

func PrependBaseURL(path *string, storageType ...StorageType) *string {
	if path == nil || *path == "" {
		return nil
	}

	if strings.HasPrefix(*path, "http://") || strings.HasPrefix(*path, "https://") {
		return path
	}
	// Default storage type
	storage := StorageLocal
	if len(storageType) > 0 {
		storage = storageType[0]
	}

	cleanedPath := strings.TrimPrefix(*path, "/")
	fullURL := BaseURL + "/" + string(storage) + "/" + cleanedPath

	return &fullURL
}

// S3 storage üçün
func PrependS3URL(path *string) *string {
	return PrependBaseURL(path, StorageS3)
}

// Local storage üçün
func PrependLocalURL(path *string) *string {
	return PrependBaseURL(path, StorageLocal)
}
