// Package objectstore wraps aws-sdk-go-v2 S3 so the identical code runs against
// MinIO locally and S3 in production, selected purely by config (a custom
// BaseEndpoint plus path-style addressing for MinIO).
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Client is an S3-API object store scoped to a single bucket.
type Client struct {
	s3     *s3.Client
	bucket string
}

// NewClient builds a client from primitive settings (kept config-agnostic like
// the other infra constructors). An empty endpoint uses AWS S3 with virtual-host
// addressing; a set endpoint plus pathStyle targets MinIO.
func NewClient(ctx context.Context, endpoint, region, bucket, accessKey, secretKey string, pathStyle bool) (*Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = pathStyle
	})
	return &Client{s3: s3Client, bucket: bucket}, nil
}

// NewKey joins path segments into a deterministic object key.
func (c *Client) NewKey(parts ...string) string {
	return strings.Join(parts, "/")
}

// Put writes an object under key.
func (c *Client) Put(ctx context.Context, key string, r io.Reader, contentType string) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        r,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// Get opens an object for reading. The caller closes the returned reader.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return out.Body, nil
}

// Delete removes an object under key. Deleting an absent key is not an error
// (the S3 API is idempotent for DeleteObject), so the dataset-retirement path
// can delete an object even when a prior cleanup already removed it.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// IsNotFound reports whether err is a missing-object result, from either the
// modeled NoSuchKey type or MinIO's non-modeled API error form. It keeps SDK-
// specific error-shape knowledge inside this wrapper so callers classify a
// missing key without importing the S3 types themselves.
func IsNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound"
	}
	return false
}

// EnsureBucket idempotently creates the configured bucket to bootstrap local
// MinIO, which starts empty. It creates directly rather than checking first, so
// two services racing to bootstrap the same empty bucket both succeed: an
// already-owned/already-exists result from a concurrent creator is treated as
// success, not a fatal error.
func (c *Client) EnsureBucket(ctx context.Context) error {
	_, err := c.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(c.bucket)})
	if err == nil {
		return nil
	}
	// A bucket we already own is idempotent success (the concurrent-bootstrap
	// race). BucketAlreadyExists is deliberately NOT treated as success: it means
	// the globally-unique name is owned by a different account, a misconfiguration
	// to surface rather than mask behind later AccessDenied errors. The
	// smithy.APIError code check also catches MinIO's non-modeled error form.
	var owned *types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "BucketAlreadyOwnedByYou" {
		return nil
	}
	return fmt.Errorf("create bucket %q: %w", c.bucket, err)
}
