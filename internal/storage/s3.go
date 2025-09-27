package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Service[T any] struct {
	client  *minio.Client
	keyFunc func(value T) string
	count   int
}

func NewS3Service[T any](keyFunc func(value T) string) (*S3Service[T], error) {
	minioEndpoint := os.Getenv("MINIO_ENDPOINT")
	minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
	minioSecretKey := os.Getenv("MINIO_SECRET_KEY")
	useSSL := os.Getenv("MINIO_USE_SSL") == "true"

	if minioEndpoint == "" || minioAccessKey == "" || minioSecretKey == "" {
		return nil, fmt.Errorf("missing one or more required environment variables")
	}

	client, err := minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioAccessKey, minioSecretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create MinIO client: %w", err)
	}

	log.Println("Connected to MinIO:", minioEndpoint)
	return &S3Service[T]{client: client, keyFunc: keyFunc}, nil
}

func (s *S3Service[T]) CreateBucket(ctx context.Context, bucketName string, location string) (bool, error) {
	exists, err := s.client.BucketExists(ctx, bucketName)
	if err != nil {
		return false, fmt.Errorf("error checking bucket existence: %v", err)
	}
	if !exists {
		err = s.client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: location})
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *S3Service[T]) StoreObject(ctx context.Context, bucketName string, value T) error {
	key := s.keyFunc(value)

	_, err := s.client.StatObject(ctx, bucketName, key, minio.StatObjectOptions{})
	if err == nil {
		log.Printf("Object '%s' already exists in bucket '%s'. Skipping.", key, bucketName)
		return nil
	}
	if minio.ToErrorResponse(err).Code != "NoSuchKey" {
		return fmt.Errorf("failed to stat object: %w", err)
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to marshal: %w", err)
	}

	_, err = s.client.PutObject(
		ctx,
		bucketName,
		key,
		bytes.NewReader(data),
		int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json"},
	)
	if err != nil {
		return fmt.Errorf("failed to store object: %w", err)
	}

	log.Printf("Stored object with key '%s' in bucket '%s'", key, bucketName)
	return nil
}

func (s *S3Service[T]) StoreFromChannel(ctx context.Context, bucketName string, values <-chan T) {
	var wg sync.WaitGroup

	for v := range values {
		wg.Add(1)
		go func(val T) {
			defer wg.Done()
			if err := s.StoreObject(ctx, bucketName, val); err != nil {
				log.Printf("Error storing object: %v", err)
			} else {
				s.count++
			}
		}(v)
	}

	wg.Wait()
	log.Printf("Stored %d objects in bucket '%s'", s.count, bucketName)
}

func (s *S3Service[T]) GetObject(ctx context.Context, bucketName, key string) (*T, error) {
	obj, err := s.client.GetObject(ctx, bucketName, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object: %w", err)
	}
	defer obj.Close()

	var value T
	if err := json.NewDecoder(obj).Decode(&value); err != nil {
		return nil, fmt.Errorf("failed to decode object: %w", err)
	}
	return &value, nil
}
