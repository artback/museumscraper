// Package storage wraps the S3/MinIO client used to persist museum records.
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// storeConcurrency bounds the number of in-flight uploads. The producer side is
// a Wikipedia crawl, which can emit thousands of museums; without a bound
// StoreFromChannel would start a goroutine and an HTTP request for every one of
// them at once.
const storeConcurrency = 16

// S3Service reads and writes values of type T as JSON objects, deriving each
// object's key from the value itself.
type S3Service[T any] struct {
	client  *minio.Client
	keyFunc func(value T) string
}

// NewS3Service builds a client from the MINIO_* environment variables.
func NewS3Service[T any](keyFunc func(value T) string) (*S3Service[T], error) {
	var (
		endpoint  = os.Getenv("MINIO_ENDPOINT")
		accessKey = os.Getenv("MINIO_ACCESS_KEY")
		secretKey = os.Getenv("MINIO_SECRET_KEY")
		useSSL    = os.Getenv("MINIO_USE_SSL") == "true"
	)

	if endpoint == "" || accessKey == "" || secretKey == "" {
		return nil, errors.New("MINIO_ENDPOINT, MINIO_ACCESS_KEY and MINIO_SECRET_KEY must all be set")
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("create MinIO client: %w", err)
	}

	log.Println("Connected to MinIO:", endpoint)
	return &S3Service[T]{client: client, keyFunc: keyFunc}, nil
}

// EnsureBucket creates bucketName in the given region if it does not exist.
func (s *S3Service[T]) EnsureBucket(ctx context.Context, bucketName, region string) error {
	exists, err := s.client.BucketExists(ctx, bucketName)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", bucketName, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: region}); err != nil {
		return fmt.Errorf("create bucket %q: %w", bucketName, err)
	}
	return nil
}

// PutObject writes value as JSON, overwriting whatever was at its key.
func (s *S3Service[T]) PutObject(ctx context.Context, bucketName string, value T) error {
	key := s.keyFunc(value)

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal object for key %q: %w", key, err)
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
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// StoreObject writes value only when no object already exists at its key. It
// reports whether a write happened.
func (s *S3Service[T]) StoreObject(ctx context.Context, bucketName string, value T) (bool, error) {
	key := s.keyFunc(value)

	switch _, err := s.client.StatObject(ctx, bucketName, key, minio.StatObjectOptions{}); {
	case err == nil:
		return false, nil
	case !isNotFound(err):
		return false, fmt.Errorf("stat object %q: %w", key, err)
	}

	if err := s.PutObject(ctx, bucketName, value); err != nil {
		return false, err
	}
	return true, nil
}

// ErrNotFound reports that an object or bucket does not exist. Reads wrap the
// backend's own error in it, so callers — including ones given a different
// implementation of the read interface — can test for absence with errors.Is
// instead of having to recognise a MinIO error response.
var ErrNotFound = errors.New("object not found")

// isNotFound reports whether err means "this object does not exist" as opposed
// to a real failure. MinIO and S3 disagree on the code, so both are accepted.
//
// The MinIO response is located with errors.As rather than with the SDK's
// ToErrorResponse, which type-asserts without unwrapping: every read here
// annotates its error with the key, and the assertion fails on the wrapper, so
// a missing object was being reported as a genuine failure.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}

	var response minio.ErrorResponse
	if !errors.As(err, &response) {
		return false
	}
	switch response.Code {
	case "NoSuchKey", "NoSuchBucket", "NotFound":
		return true
	default:
		return response.StatusCode == http.StatusNotFound
	}
}

// asNotFound wraps a backend absence error in ErrNotFound, leaving real
// failures untouched.
func asNotFound(err error) error {
	if err != nil && isNotFound(err) && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// StoreFromChannel writes every value from values until the channel closes or
// ctx is cancelled, and returns the number of objects actually written.
// Uploads run concurrently, bounded by storeConcurrency.
func (s *S3Service[T]) StoreFromChannel(ctx context.Context, bucketName string, values <-chan T) int {
	var (
		wg      sync.WaitGroup
		stored  atomic.Int64
		skipped atomic.Int64
		failed  atomic.Int64
		slots   = make(chan struct{}, storeConcurrency)
	)

loop:
	for v := range values {
		// Acquiring a slot doubles as the cancellation check.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break loop
		}

		wg.Add(1)
		go func(val T) {
			defer wg.Done()
			defer func() { <-slots }()

			switch written, err := s.StoreObject(ctx, bucketName, val); {
			case err != nil:
				failed.Add(1)
				log.Printf("Error storing object: %v", err)
			case written:
				stored.Add(1)
			default:
				skipped.Add(1)
			}
		}(v)
	}

	wg.Wait()
	log.Printf("Stored %d objects in bucket %q (%d already present, %d failed)",
		stored.Load(), bucketName, skipped.Load(), failed.Load())
	return int(stored.Load())
}

// EachObject calls fn for every object under prefix, decoding each into a T.
//
// Objects are fetched concurrently, bounded by storeConcurrency, because a full
// catalogue is tens of thousands of small objects and fetching them one at a
// time is dominated by round trips. fn may be called from several goroutines,
// so it must be safe for concurrent use.
func (s *S3Service[T]) EachObject(ctx context.Context, bucketName, prefix string, fn func(key string, value T)) error {
	var (
		wg     sync.WaitGroup
		slots  = make(chan struct{}, storeConcurrency)
		failed atomic.Int64
	)

	// Wait on every exit, not just the successful one.
	//
	// A listing error can arrive after earlier pages have already been
	// dispatched, so returning on it left up to storeConcurrency goroutines
	// still calling fn. Callers reasonably treat a returned error as "nothing
	// is running any more" and go on to read what fn was writing: reindex
	// logs the error, carries on, and ranges the map its callback is still
	// filling, which is an unrecoverable concurrent map read and write.
	//
	// The explicit Wait below stays where it is — the failed count is read
	// after it, and this deferred one runs too late for that.
	defer wg.Wait()

	objects := s.client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	for object := range objects {
		if object.Err != nil {
			return fmt.Errorf("list %s/%s: %w", bucketName, prefix, object.Err)
		}

		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}

		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			defer func() { <-slots }()

			value, err := s.GetObject(ctx, bucketName, key)
			if err != nil {
				failed.Add(1)
				log.Printf("Skipping %s: %v", key, err)
				return
			}
			fn(key, *value)
		}(object.Key)
	}

	wg.Wait()
	if n := failed.Load(); n > 0 {
		log.Printf("%d objects under %s could not be read", n, prefix)
	}
	return nil
}

// PutJSON writes value as JSON at an explicit key, bypassing keyFunc. It is
// used for derived objects such as the geo index, whose keys come from the data
// rather than from a single record.
func (s *S3Service[T]) PutJSON(ctx context.Context, bucketName, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal object for key %q: %w", key, err)
	}

	_, err = s.client.PutObject(
		ctx, bucketName, key,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json"},
	)
	if err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// GetJSON fetches and decodes an arbitrary JSON object at key. A missing
// object yields an error wrapping ErrNotFound.
func (s *S3Service[T]) GetJSON(ctx context.Context, bucketName, key string, out any) error {
	obj, err := s.client.GetObject(ctx, bucketName, key, minio.GetObjectOptions{})
	if err != nil {
		return asNotFound(fmt.Errorf("get object %q: %w", key, err))
	}
	defer obj.Close()

	if err := json.NewDecoder(obj).Decode(out); err != nil {
		// MinIO defers the request until the body is read, so a missing object
		// surfaces here rather than from GetObject.
		return asNotFound(fmt.Errorf("decode object %q: %w", key, err))
	}
	return nil
}

// IsNotFound reports whether err means the object or bucket does not exist,
// as opposed to a real failure.
func IsNotFound(err error) bool { return isNotFound(err) }

// GetObject fetches and decodes the JSON object at key.
func (s *S3Service[T]) GetObject(ctx context.Context, bucketName, key string) (*T, error) {
	obj, err := s.client.GetObject(ctx, bucketName, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, asNotFound(fmt.Errorf("get object %q: %w", key, err))
	}
	defer obj.Close()

	var value T
	if err := json.NewDecoder(obj).Decode(&value); err != nil {
		return nil, asNotFound(fmt.Errorf("decode object %q: %w", key, err))
	}
	return &value, nil
}

// RemoveObject deletes the object at key. Deleting something that is not there
// is not an error: callers prune by listing and then removing, and an object
// that vanished between the two steps has already reached the wanted state.
func (s *S3Service[T]) RemoveObject(ctx context.Context, bucketName, key string) error {
	err := s.client.RemoveObject(ctx, bucketName, key, minio.RemoveObjectOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("remove object %q: %w", key, err)
	}
	return nil
}

// ListKeys returns the keys under prefix, in the order the backend lists them,
// which for S3 and MinIO is lexicographic.
//
// It fetches no objects. Callers that need one datum derivable from a key —
// the newest of a set of timestamp-named objects, the highest of a set of
// zero-padded version numbers — were downloading and decoding the whole prefix
// to compute it, which on a scheduler tick is an object-storage request storm
// for information the listing already carried.
func (s *S3Service[T]) ListKeys(ctx context.Context, bucketName, prefix string) ([]string, error) {
	var keys []string

	objects := s.client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})
	for object := range objects {
		if object.Err != nil {
			return nil, fmt.Errorf("list %s/%s: %w", bucketName, prefix, object.Err)
		}
		keys = append(keys, object.Key)
	}
	return keys, nil
}
