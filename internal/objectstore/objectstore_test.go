package objectstore_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/arborette/arborette/internal/objectstore"
	"github.com/arborette/arborette/internal/testutil"
)

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)

	client, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket, cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	key := client.NewKey("test", testutil.NewID(t), "data.bin")
	want := []byte("arborette object store round-trip")
	if err := client.Put(ctx, key, bytes.NewReader(want), "application/octet-stream"); err != nil {
		t.Fatalf("put: %v", err)
	}

	r, err := client.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}
}

// TestEnsureBucketIdempotent deterministically exercises the already-owned
// branch: the second call must succeed against the bucket the first created,
// which is what lets two services race to bootstrap the same bucket.
func TestEnsureBucketIdempotent(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)

	client, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket, cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.EnsureBucket(ctx); err != nil {
		t.Fatalf("first EnsureBucket: %v", err)
	}
	if err := client.EnsureBucket(ctx); err != nil {
		t.Fatalf("second EnsureBucket (idempotent already-owned path): %v", err)
	}
}
