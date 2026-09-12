// Package blob stores attachment bytes outside the benchmark database.
package blob

import (
	"context"
	"fmt"
	"io"
	"net/url"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 delegates multipart uploads and request signing to the S3 client.
type S3 struct {
	client *minio.Client
	bucket string
}

func NewS3(endpoint, bucket, region string, creds *credentials.Credentials) (*S3, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("artifact endpoint must be an http or https origin")
	}
	client, err := minio.New(u.Host, &minio.Options{Creds: creds, Secure: u.Scheme == "https", Region: region})
	if err != nil {
		return nil, err
	}
	return &S3{client: client, bucket: bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, body io.Reader, sizeHint int64, mediaType string) (int64, error) {
	// The SDK sizes multipart buffers from Content-Length when supplied.
	// Unknown-length streams use its default sizing for the S3 object maximum.
	info, err := s.client.PutObject(ctx, s.bucket, key, body, sizeHint, minio.PutObjectOptions{ContentType: mediaType})
	return info.Size, err
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy. Resolve storage errors before committing HTTP headers.
	if _, err := object.Stat(); err != nil {
		_ = object.Close()
		return nil, err
	}
	return object, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
