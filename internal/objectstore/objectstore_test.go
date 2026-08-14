package objectstore_test

import (
	"bytes"
	"context"
	"io"
	"slices"
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
	t.Cleanup(func() {
		if err := client.Delete(context.WithoutCancel(ctx), key); err != nil {
			t.Errorf("cleanup object %q: %v", key, err)
		}
	})
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

// TestWipeDeletesEveryObject puts enough keys to cross a DeleteObjects batch
// boundary (ListKeys/Wipe batch by 1000), asserts both keys appear in the
// bucket's listing, then wipes and asserts the bucket is empty and a second
// wipe is an idempotent no-op. The counts are relative to the keys this test
// itself created (the bucket is shared), so nothing is asserted in absolute
// terms -- only that its own keys vanish and Wipe's reported count matches what
// ListKeys observed before the wipe.
func TestWipeDeletesEveryObject(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)

	client, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket, cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	const own = 1005 // one past the 1000-key batch boundary
	ownKeys := make(map[string]bool, own)
	for i := 0; i < own; i++ {
		key := client.NewKey("wipe-test", testutil.NewID(t), "data.bin")
		if err := client.Put(ctx, key, bytes.NewReader([]byte("wipe fixture")), "application/octet-stream"); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		ownKeys[key] = true
	}

	keys, err := client.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list after put: %v", err)
	}
	for key := range ownKeys {
		if !slices.Contains(keys, key) {
			t.Fatalf("own key %q missing from listing", key)
		}
	}
	before := len(keys)

	wiped, err := client.Wipe(ctx)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if wiped != before {
		t.Fatalf("wipe deleted %d keys, want the %d that ListKeys observed", wiped, before)
	}

	keys, err = client.ListKeys(ctx)
	if err != nil {
		t.Fatalf("list after wipe: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("bucket has %d keys after wipe, want 0", len(keys))
	}

	wipedAgain, err := client.Wipe(ctx)
	if err != nil {
		t.Fatalf("second wipe: %v", err)
	}
	if wipedAgain != 0 {
		t.Fatalf("second wipe deleted %d keys, want 0", wipedAgain)
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
