package services

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Uploader — Laravel Storage::disk('s3') qarşılığı (voice + media upload).
// Digər Go servislərindəki (cosmetics/community) s3.go ilə eyni stildə.
type S3Uploader struct {
	client *s3.Client
	bucket string
}

// NewS3Uploader — S3/MinIO/Hetzner client. bucket boş olduqda no-op uploader.
// pathStyle Laravel `AWS_USE_PATH_STYLE_ENDPOINT` ilə eynidir: Hetzner Object
// Storage virtual-hosted işlədir (false) → bucket.endpoint; MinIO isə true.
func NewS3Uploader(bucket, region, endpoint, accessKey, secretKey string, pathStyle bool) *S3Uploader {
	if bucket == "" {
		return &S3Uploader{}
	}
	cfg, err := awscfg.LoadDefaultConfig(context.TODO(),
		awscfg.WithRegion(region),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return &S3Uploader{bucket: bucket}
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = pathStyle
		}
	})
	return &S3Uploader{client: client, bucket: bucket}
}

// Enabled — S3 client konfiqurasiya olunubmu.
func (u *S3Uploader) Enabled() bool { return u.client != nil }

// Put — Laravel Storage::disk('s3')->putFileAs(). Verilmiş açara xam baytları
// qoyur (ContentType ilə).
func (u *S3Uploader) Put(key string, body []byte, contentType string) error {
	if u.client == nil {
		// Issue 43: əvvəl `nil` (yəni UĞUR) qaytarırdı. S3 konfiqurasiya
		// olunmayıbsa (bucket boş, ya da `LoadDefaultConfig` xəta verib)
		// yükləmə SƏSSİZCƏ heç nə etmir, amma handler istifadəçiyə etibarlı
		// görünən bir URL qaytarırdı → şəkil/video "sonsuza qədər yüklənir",
		// heç bir xəta görünmür. İndi səhv AÇIQ şəkildə qayıdır.
		return errors.New("s3: uploader konfiqurasiya olunmayıb (bucket/credentials yoxdur)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	in := &s3.PutObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	_, err := u.client.PutObject(ctx, in)
	return err
}

// Delete — Issue 56: verilmiş açarı bucket-dan silir. Sahibsiz media
// təmizləyicisi (services/media_gc.go) və uğursuz yükləmə yolları işlədir.
// Əvvəl `S3Uploader`-də silmə metodu ÜMUMİYYƏTLƏ yox idi — yəni bir dəfə
// yazılan obyekti proqramla silmək mümkün deyildi.
func (u *S3Uploader) Delete(key string) error {
	if u.client == nil {
		return errors.New("s3: uploader konfiqurasiya olunmayıb (bucket/credentials yoxdur)")
	}
	if key == "" {
		return errors.New("s3: boş açar")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := u.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
	})
	return err
}

// Exists — Laravel Storage::disk('s3')->exists($key). uploadVoice put-dan sonra
// obyektin həqiqətən yazıldığını təsdiqləmək üçün (fail loudly).
func (u *S3Uploader) Exists(key string) bool {
	if u.client == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := u.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
	})
	return err == nil
}
