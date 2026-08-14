// Package objectstore wraps aws-sdk-go-v2 S3 so the identical code runs against
// MinIO locally and S3 in production, selected purely by config (a custom
// BaseEndpoint plus path-style addressing for MinIO).
package objectstore

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
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
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
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
		o.APIOptions = append(o.APIOptions, contentMD5Middleware)
	})
	return &Client{s3: s3Client, bucket: bucket}, nil
}

// contentMD5Middleware stamps the Content-MD5 header onto multi-object-delete
// requests. MinIO requires the header (it refuses DeleteObjects without it), and
// aws-sdk-go-v2 leaves the header off the wire even though the API documents it
// as required. The middleware runs after serialization, when the request body
// holds the XML payload, and only touches requests for the ?delete endpoint so
// every other operation is untouched.
func contentMD5Middleware(stack *middleware.Stack) error {
	return stack.Serialize.Add(middleware.SerializeMiddlewareFunc(
		"ContentMD5ForDeleteObjects",
		func(ctx context.Context, in middleware.SerializeInput, next middleware.SerializeHandler) (
			middleware.SerializeOutput, middleware.Metadata, error,
		) {
			req, ok := in.Request.(*smithyhttp.Request)
			if !ok || !req.URL.Query().Has("delete") {
				return next.HandleSerialize(ctx, in)
			}
			body, err := io.ReadAll(req.GetStream())
			if err != nil {
				return middleware.SerializeOutput{}, middleware.Metadata{},
					fmt.Errorf("read multi-delete payload for checksum: %w", err)
			}
			sum := md5.Sum(body)
			req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
			req, err = req.SetStream(bytes.NewReader(body))
			if err != nil {
				return middleware.SerializeOutput{}, middleware.Metadata{},
					fmt.Errorf("rewind multi-delete payload: %w", err)
			}
			in.Request = req
			return next.HandleSerialize(ctx, in)
		},
	), middleware.After)
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

// ListKeys returns every object key in the bucket. It is the listing the wipe
// and the cleanliness gate both need, and the single place the driver's
// continuation-token paging lives so callers never see a partial page.
func (c *Client) ListKeys(ctx context.Context) ([]string, error) {
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
	})
	var keys []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects in %q: %w", c.bucket, err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, *obj.Key)
		}
	}
	return keys, nil
}

// Wipe removes every object the bucket holds. It is the object-store arm of the
// operator reset behind cmd/cleanup clean, deleting in DeleteObjects batches of
// up to a thousand keys. An empty bucket has no keys to delete, so wiping an
// already-clean bucket succeeds and changes nothing (idempotent). Unlike the
// per-key Delete, this never touches the bucket itself, so a concurrently
// bootstrapping service's EnsureBucket race is unaffected.
func (c *Client) Wipe(ctx context.Context) (int, error) {
	keys, err := c.ListKeys(ctx)
	if err != nil {
		return 0, err
	}
	for start := 0; start < len(keys); start += 1000 {
		end := start + 1000
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[start:end]
		identifiers := make([]types.ObjectIdentifier, len(batch))
		for i, key := range batch {
			identifiers[i] = types.ObjectIdentifier{Key: aws.String(key)}
		}
		if _, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{Objects: identifiers},
		}); err != nil {
			return 0, fmt.Errorf("delete %d objects in %q: %w", len(batch), c.bucket, err)
		}
	}
	return len(keys), nil
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
