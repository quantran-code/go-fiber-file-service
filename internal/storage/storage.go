// Package storage wraps the S3-compatible object store the file service uses.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/quantran-code/go-fiber-file-service/internal/config"
)

// Object-store failures the HTTP layer can map onto responses.
var (
	ErrNotConfigured = errors.New("storage not configured")
	ErrNotFound      = errors.New("object not found")
	ErrUnavailable   = errors.New("storage unavailable")
)

// ObjectInfo is the metadata of one stored object.
type ObjectInfo struct {
	Key         string
	Size        int64
	ContentType string
	ETag        string
}

// Object is one readable object. The caller closes Body.
type Object struct {
	Info ObjectInfo
	Body io.ReadCloser
}

// Store is the object-store seam the handlers depend on. It is deliberately
// small: the feature tickets build file streaming, hashing and presigned
// uploads on top of it without needing a different client in their tests.
type Store interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	// Get reads length bytes starting at offset. A length of zero or less reads
	// to the end of the object.
	Get(ctx context.Context, key string, offset, length int64) (*Object, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	PresignPut(ctx context.Context, key string, ttl time.Duration, contentType string) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

// MinIO is the minio-go implementation of Store.
type MinIO struct {
	client *minio.Client
	bucket string
}

// NewMinIO builds a client for the configured S3-compatible endpoint. It does
// not dial the endpoint, so an unreachable store only fails when an operation
// runs.
func NewMinIO(cfg config.S3) (*MinIO, error) {
	if !cfg.Configured() {
		return nil, ErrNotConfigured
	}

	host, secure, err := endpointHost(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	lookup := minio.BucketLookupAuto
	if cfg.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}

	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("building the object-store client: %w", err)
	}

	return &MinIO{client: client, bucket: cfg.Bucket}, nil
}

func endpointHost(endpoint string) (string, bool, error) {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return "", false, ErrNotConfigured
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("parsing the S3 endpoint: %w", err)
	}
	if parsed.Host == "" {
		return "", false, fmt.Errorf("parsing the S3 endpoint: %q carries no host", endpoint)
	}
	return parsed.Host, parsed.Scheme == "https", nil
}

// Put writes the whole object in one call.
func (s *MinIO) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, body, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("%w: putting %q: %v", ErrUnavailable, key, err)
	}
	return nil
}

// Get reads the requested byte range. The caller owns the returned Body.
func (s *MinIO) Get(ctx context.Context, key string, offset, length int64) (*Object, error) {
	options := minio.GetObjectOptions{}
	if offset > 0 || length > 0 {
		end := int64(-1)
		if length > 0 {
			end = offset + length - 1
		}
		if end < 0 {
			// Open-ended ranges are expressed by a very large end; the S3 API
			// clamps it to the object size.
			end = int64(^uint64(0) >> 1)
		}
		options.SetRange(offset, end)
	}

	object, err := s.client.GetObject(ctx, s.bucket, key, options)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %q: %v", classify(err), key, err)
	}

	info, err := object.Stat()
	if err != nil {
		_ = object.Close()
		return nil, fmt.Errorf("%w: reading %q: %v", classify(err), key, err)
	}

	return &Object{
		Info: ObjectInfo{
			Key:         key,
			Size:        info.Size,
			ContentType: info.ContentType,
			ETag:        info.ETag,
		},
		Body: object,
	}, nil
}

// Stat returns one object's metadata.
func (s *MinIO) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("%w: stating %q: %v", classify(err), key, err)
	}
	return ObjectInfo{Key: key, Size: info.Size, ContentType: info.ContentType, ETag: info.ETag}, nil
}

// PresignPut signs a PUT for the object under key. The content type is not part
// of the signature; the upload page sends it on the PUT and the completion step
// verifies the stored object's metadata.
func (s *MinIO) PresignPut(ctx context.Context, key string, ttl time.Duration, _ string) (string, error) {
	presigned, err := s.client.PresignedPutObject(ctx, s.bucket, key, ttl)
	if err != nil {
		return "", fmt.Errorf("%w: presigning PUT %q: %v", ErrUnavailable, key, err)
	}
	return presigned.String(), nil
}

// PresignGet signs a GET for the object under key.
func (s *MinIO) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	presigned, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("%w: presigning GET %q: %v", ErrUnavailable, key, err)
	}
	return presigned.String(), nil
}

// Delete removes the object under key.
func (s *MinIO) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("%w: deleting %q: %v", ErrUnavailable, key, err)
	}
	return nil
}

func classify(err error) error {
	response := minio.ToErrorResponse(err)
	switch response.Code {
	case "NoSuchKey", "NoSuchObject", "NotFound":
		return ErrNotFound
	}
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return ErrUnavailable
}
