// Package objectstore stores original document files in an S3-compatible bucket: Amazon S3
// in production, RustFS locally. The bucket is private; browsers only ever reach it through
// short-lived presigned URLs that the Core API signs after checking access.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var ErrNotFound = errors.New("object not found")

type Config struct {
	// Endpoint is how the API reaches the store, e.g. http://localhost:9000.
	Endpoint string
	// PublicEndpoint is the address browsers use. Presigned URLs are signed for this host,
	// so it must be the one the browser will actually call. Defaults to Endpoint.
	PublicEndpoint string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
}

type Store struct {
	bucket    string
	client    *s3.Client
	presigner *s3.PresignClient
}

func New(cfg Config) *Store {
	publicEndpoint := cfg.PublicEndpoint
	if publicEndpoint == "" {
		publicEndpoint = cfg.Endpoint
	}
	return &Store{
		bucket:    cfg.Bucket,
		client:    newClient(cfg, cfg.Endpoint),
		presigner: s3.NewPresignClient(newClient(cfg, publicEndpoint)),
	}
}

func newClient(cfg Config, endpoint string) *s3.Client {
	return s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(endpoint),
		// Path-style URLs (host/bucket/key) work with S3-compatible servers and local hosts.
		UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: cfg.AccessKey, SecretAccessKey: cfg.SecretKey}, nil
		}),
	})
}

// CheckBucket fails if the bucket is missing or unreachable, so a misconfigured store stops
// the server at startup.
func (s *Store) CheckBucket(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("check bucket %q: %w", s.bucket, err)
	}
	return nil
}

// EnsureBucket creates the bucket if it does not exist. Only tests use it: real buckets are
// provisioned outside the application (compose locally, Terraform in AWS).
func (s *Store) EnsureBucket(ctx context.Context) error {
	if s.CheckBucket(ctx) == nil {
		return nil
	}
	if _, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("create bucket %q: %w", s.bucket, err)
	}
	return nil
}

// PresignedUpload is what a browser needs to PUT one file straight to the store.
type PresignedUpload struct {
	URL       string
	Method    string
	Headers   map[string]string // headers the browser must send exactly as given
	ExpiresAt time.Time
}

// PresignUpload signs a PUT for exactly this key, content type and size. The store rejects
// the upload if any of them differ, so the URL cannot be reused for another file or size.
func (s *Store) PresignUpload(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (PresignedUpload, error) {
	req, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return PresignedUpload{}, fmt.Errorf("presign upload: %w", err)
	}
	return PresignedUpload{
		URL:    req.URL,
		Method: req.Method,
		// Content-Length and Host are signed too, but browsers set those themselves.
		Headers:   map[string]string{"Content-Type": contentType},
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

// PresignDownload signs a GET that makes the browser save the file under filename instead of
// rendering it, with the content type we verified rather than whatever was uploaded.
func (s *Store) PresignDownload(ctx context.Context, key, filename, contentType string, ttl time.Duration) (string, error) {
	req, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		// FormatMediaType quotes the name and RFC 2231-encodes non-ASCII characters, so a
		// filename cannot inject extra header parameters.
		ResponseContentDisposition: aws.String(mime.FormatMediaType("attachment", map[string]string{"filename": filename})),
		ResponseContentType:        aws.String(contentType),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign download: %w", err)
	}
	return req.URL, nil
}

// Size returns the stored object's size in bytes, or ErrNotFound.
func (s *Store) Size(ctx context.Context, key string) (int64, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if isNotFound(err) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("head object: %w", err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

// Open streams the object's content. The caller must close the reader.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if isNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	return out.Body, nil
}

// Delete removes the object. Deleting a missing object succeeds, so retries are safe.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound
}
