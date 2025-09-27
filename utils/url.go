package utils

import "strings"

const BaseURL = "https://api.beanpon.com"

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
