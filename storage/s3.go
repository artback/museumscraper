package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"museum/models"
	"os"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Service is a client for S3-compatible storage.
type S3Service struct {
	client *minio.Client
	count  int
}

// NewS3Service initializes and returns a new S3 storage service.
// It connects to the MinIO server using credentials from environment variables.
func NewS3Service() (*S3Service, error) {
	minioEndpoint := os.Getenv("MINIO_ENDPOINT")
	minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
	minioSecretKey := os.Getenv("MINIO_SECRET_KEY")
	useSSL := os.Getenv("MINIO_USE_SSL") == "true"

	if minioEndpoint == "" || minioAccessKey == "" || minioSecretKey == "" {
		return nil, fmt.Errorf("missing one or more required environment variables: MINIO_ENDPOINT, MINIO_ACCESS_KEY, MINIO_SECRET_KEY")
	}

	minioClient, err := minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioAccessKey, minioSecretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create MinIO client: %v", err)
	}

	log.Println("Successfully connected to MinIO endpoint:", minioEndpoint)
	return &S3Service{client: minioClient, count: 0}, nil
}

func (s *S3Service) CreateBucket(ctx context.Context, bucketName string, location string) (bool, error) {
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

// StoreMuseumsFromChannel reads Museum objects from a channel and stores each one
// in the specified S3 bucket. It is a more scalable approach for handling a stream of data.
func (s *S3Service) StoreMuseumsFromChannel(ctx context.Context, bucketName string, museums <-chan models.Museum) {
	var wg sync.WaitGroup

	for museum := range museums {
		wg.Add(1)
		go func(m models.Museum) {
			defer wg.Done()
			err := s.storeSingleMuseum(ctx, bucketName, m)
			s.count++
			if err != nil {
				log.Printf("Error storing museum '%s': %v", m.Name, err)
			}
		}(museum)
	}

	wg.Wait()
	log.Printf("Finished storing all museums from the channel. Count %d \n", s.count)
}

// storeSingleMuseum is a helper function to store a single Museum object.
// It will not overwrite a file if it already exists.
func (s *S3Service) storeSingleMuseum(ctx context.Context, bucketName string, museum models.Museum) error {
	objectKey := fmt.Sprintf("raw_data/%s/%s.json", sanitizeKey(museum.Country), sanitizeKey(museum.Name))

	// Check if the object already exists.
	_, err := s.client.StatObject(ctx, bucketName, objectKey, minio.StatObjectOptions{})
	if err == nil {
		// The object was found, which means it already exists. We can ignore the write.
		log.Printf("Museum file for '%s' already exists in bucket '%s'. Ignoring write operation.", museum.Name, bucketName)
		return nil
	}

	// The object was not found, which is the expected behavior for a new object.
	// Check if the error is specifically a "NoSuchKey" error. If not, it's a different issue.
	if minio.ToErrorResponse(err).Code != "NoSuchKey" {
		return fmt.Errorf("failed to check for existing object: %v", err)
	}

	// If we get here, the object does not exist, so we can proceed with the put.
	data, err := json.Marshal(museum)
	if err != nil {
		return fmt.Errorf("failed to marshal museum to JSON: %v", err)
	}

	_, err = s.client.PutObject(
		ctx,
		bucketName,
		objectKey,
		bytes.NewReader(data),
		int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json"},
	)
	if err != nil {
		return fmt.Errorf("failed to store object in S3: %v", err)
	}

	log.Printf("Successfully stored new museum '%s' in bucket '%s' with key '%s'", museum.Name, bucketName, objectKey)
	return nil
}

func (s *S3Service) GetMuseumObject(ctx context.Context, bucketName string, objectKey string) (*models.Museum, error) {
	object, err := s.client.GetObject(ctx, bucketName, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from S3: %v", err)
	}
	defer object.Close()

	// Use json.NewDecoder to stream the JSON directly from the reader.
	var museum models.Museum
	if err := json.NewDecoder(object).Decode(&museum); err != nil {
		return nil, fmt.Errorf("failed to decode JSON from stream: %v", err)
	}

	log.Printf("Successfully retrieved museum '%s' from bucket '%s' with key '%s'", museum.Name, bucketName, objectKey)
	return &museum, nil
}

// GetMuseum retrieves a JSON object from S3 and unmarshals it into a Museum struct.
func (s *S3Service) GetMuseum(ctx context.Context, bucketName string, country, name string) (*models.Museum, error) {
	objectKey := fmt.Sprintf("raw_data/%s/%s.json", sanitizeKey(country), sanitizeKey(name))
	return s.GetMuseumObject(ctx, bucketName, objectKey)
}

// sanitizeKey replaces non-alphanumeric characters with hyphens to create a valid object key.
func sanitizeKey(s string) string {
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ToLower(s)
	return s
}
