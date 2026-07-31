package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newStageCache(t *testing.T, maxBytes int64) *StageCache {
	t.Helper()
	c, err := NewStageCache(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("new stage cache: %v", err)
	}
	return c
}

// writeArtifact writes n bytes to dest, standing in for a real download+convert.
func writeArtifact(dest string, n int64) error {
	return os.WriteFile(dest, make([]byte, n), 0o644)
}

// sizedFill returns a FillFunc that writes n bytes to dest and counts invocations.
func sizedFill(n int64, count *atomic.Int32) FillFunc {
	return func(_ context.Context, _, dest string) error {
		if count != nil {
			count.Add(1)
		}
		return writeArtifact(dest, n)
	}
}

// gatedFill returns a FillFunc that blocks until proceed is closed, so a test can
// hold a fill in flight while it arranges concurrent Acquires against it. It closes
// started exactly once when the fill begins, counts invocations in fills (nil to
// skip), and runs tail(dest) to produce the outcome (write an artifact, or error).
func gatedFill(fills *atomic.Int32, tail func(dest string) error) (fill FillFunc, started, proceed chan struct{}) {
	started = make(chan struct{})
	proceed = make(chan struct{})
	var once sync.Once
	fill = func(_ context.Context, _, dest string) error {
		if fills != nil {
			fills.Add(1)
		}
		once.Do(func() { close(started) })
		<-proceed
		return tail(dest)
	}
	return fill, started, proceed
}

func refcountUnder(t *testing.T, c *StageCache, ref string) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[ref]
	if !ok {
		return -1
	}
	return e.refcount
}

// TestStageCacheHitAmortizes proves a second Acquire for a cached ref serves the
// same artifact without a second fill -- the amortization the cache exists for.
func TestStageCacheHitAmortizes(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 1<<20)
	var fills atomic.Int32
	fill := sizedFill(100, &fills)

	p1, r1, err := c.Acquire(ctx, "ref", 100, fill)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	r1()

	p2, r2, err := c.Acquire(ctx, "ref", 100, fill)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	r2()

	if fills.Load() != 1 {
		t.Fatalf("fill ran %d times, want 1 (second call must hit)", fills.Load())
	}
	if p1 != p2 {
		t.Fatalf("hit served a different path: %q vs %q", p1, p2)
	}
	if c.Fills() != 1 || c.Hits() != 1 {
		t.Fatalf("counters = fills %d hits %d, want 1/1", c.Fills(), c.Hits())
	}
}

// TestStageCacheLRUEviction proves the least-recently-used unreferenced entry is
// evicted first when a new reservation needs room, and a more-recently-used one
// survives.
func TestStageCacheLRUEviction(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 250)
	var fills atomic.Int32
	fill := sizedFill(100, &fills)

	for _, ref := range []string{"a", "b"} {
		_, rel, err := c.Acquire(ctx, ref, 100, fill)
		if err != nil {
			t.Fatalf("acquire %s: %v", ref, err)
		}
		rel()
	}

	// resident = a(100) + b(100). Admitting c needs eviction: a is the oldest
	// unreferenced entry, so it goes and b survives.
	_, rel, err := c.Acquire(ctx, "c", 100, fill)
	if err != nil {
		t.Fatalf("acquire c: %v", err)
	}
	rel()
	if fills.Load() != 3 {
		t.Fatalf("fills=%d want 3 (a,b,c)", fills.Load())
	}

	// b is still cached: a hit, no fill.
	if _, rel, err := c.Acquire(ctx, "b", 100, fill); err != nil {
		t.Fatalf("re-acquire b: %v", err)
	} else {
		rel()
	}
	if fills.Load() != 3 {
		t.Fatalf("b should have hit, fills=%d want 3", fills.Load())
	}

	// a was evicted: re-acquiring it fills again.
	if _, rel, err := c.Acquire(ctx, "a", 100, fill); err != nil {
		t.Fatalf("re-acquire a: %v", err)
	} else {
		rel()
	}
	if fills.Load() != 4 {
		t.Fatalf("a should have missed, fills=%d want 4", fills.Load())
	}
}

// TestStageCacheReferencedEntrySurvivesEviction proves an entry with a live holder
// is never evicted: a distinct-ref reservation that cannot fit around it bypasses
// rather than deleting the file a query is reading.
func TestStageCacheReferencedEntrySurvivesEviction(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 150)

	pathA, relA, err := c.Acquire(ctx, "a", 100, sizedFill(100, nil))
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer relA()

	var bFills atomic.Int32
	_, _, err = c.Acquire(ctx, "b", 100, sizedFill(100, &bFills))
	if !errors.Is(err, errCacheBypass) {
		t.Fatalf("acquire b err=%v, want errCacheBypass (a is pinned)", err)
	}
	if bFills.Load() != 0 {
		t.Fatalf("b's fill ran %d times, want 0 (bypass precedes download)", bFills.Load())
	}
	if _, err := os.Stat(pathA); err != nil {
		t.Fatalf("pinned entry a was deleted: %v", err)
	}
}

// TestStageCacheConcurrentDedupAndRefcounts proves N concurrent first-requesters
// for one ref run the fill exactly once yet each receive an independent release,
// and the entry survives eviction pressure until the last release.
func TestStageCacheConcurrentDedupAndRefcounts(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 1<<20)
	const n = 4

	var fills atomic.Int32
	fill, started, proceed := gatedFill(&fills, func(dest string) error { return writeArtifact(dest, 100) })

	type result struct {
		path string
		rel  func()
		err  error
	}
	results := make(chan result, n)
	acquire := func() { p, r, e := c.Acquire(ctx, "ref", 100, fill); results <- result{p, r, e} }

	go acquire()
	<-started // the leader's fill is in flight; waiters now attach to it
	for i := 0; i < n-1; i++ {
		go acquire()
	}
	// The leader's call stays in flight until proceed closes, so an Acquire that
	// misses the fast path parks as a waiter rather than starting a second fill.
	close(proceed)

	got := make([]result, 0, n)
	for i := 0; i < n; i++ {
		got = append(got, <-results)
	}
	for _, r := range got {
		if r.err != nil {
			t.Fatalf("acquire err: %v", r.err)
		}
	}
	if fills.Load() != 1 {
		t.Fatalf("fill ran %d times, want 1 (fill group dedups)", fills.Load())
	}
	if rc := refcountUnder(t, c, "ref"); rc != n {
		t.Fatalf("refcount=%d, want %d independent refs", rc, n)
	}

	for i := 0; i < n-1; i++ {
		got[i].rel()
	}
	if rc := refcountUnder(t, c, "ref"); rc != 1 {
		t.Fatalf("after %d releases refcount=%d, want 1", n-1, rc)
	}
	c.mu.Lock()
	if c.evictOneLocked() {
		t.Fatalf("evicted an entry still held by its last reference")
	}
	c.mu.Unlock()

	got[n-1].rel()
	if rc := refcountUnder(t, c, "ref"); rc != 0 {
		t.Fatalf("after last release refcount=%d, want 0", rc)
	}
}

// TestStageCacheLeaderOnlyReservation proves the reservation is charged once by the
// group's leader, not once per caller: N concurrent same-ref cold requests whose
// per-caller reservations would sum past the cap all succeed on one fill.
func TestStageCacheLeaderOnlyReservation(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 150) // one 100-byte reservation fits; three would not
	const n = 3

	var fills atomic.Int32
	fill, started, proceed := gatedFill(&fills, func(dest string) error { return writeArtifact(dest, 100) })

	errs := make(chan error, n)
	acquire := func() {
		_, rel, err := c.Acquire(ctx, "ref", 100, fill)
		if rel != nil {
			rel()
		}
		errs <- err
	}
	go acquire()
	<-started
	for i := 0; i < n-1; i++ {
		go acquire()
	}
	close(proceed)

	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("acquire %d bypassed (per-caller reservation would): %v", i, err)
		}
	}
	if fills.Load() != 1 {
		t.Fatalf("fill ran %d times, want 1", fills.Load())
	}
	c.mu.Lock()
	reserved := c.reserved
	c.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("reserved=%d after settle, want 0", reserved)
	}
}

// TestStageCacheReservationInvariant proves resident + reserved never exceeds
// maxBytes under concurrent distinct-ref cold fills: the strict disk invariant the
// R22/R28 "designed together" contract rests on.
func TestStageCacheReservationInvariant(t *testing.T) {
	ctx := context.Background()
	const maxBytes = 250
	c := newStageCache(t, maxBytes)

	var violated atomic.Bool
	var peak atomic.Int64
	sample := func() {
		c.mu.Lock()
		total := c.resident + c.reserved
		c.mu.Unlock()
		for {
			old := peak.Load()
			if total <= old || peak.CompareAndSwap(old, total) {
				break
			}
		}
		if total > maxBytes {
			violated.Store(true)
		}
	}
	fill := func(_ context.Context, _, dest string) error {
		sample() // the reservation is already charged here -- catch a peak overshoot
		return writeArtifact(dest, 100)
	}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := "ref-" + string(rune('a'+i))
			_, rel, err := c.Acquire(ctx, ref, 100, fill)
			if err == nil {
				rel()
			} else if !errors.Is(err, errCacheBypass) {
				t.Errorf("unexpected acquire error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if violated.Load() {
		t.Fatalf("resident+reserved peaked at %d, exceeding maxBytes=%d", peak.Load(), maxBytes)
	}
}

// TestStageCacheOverBudgetSingletonBypasses proves a reservation larger than the
// whole budget bypasses before the fill runs -- no orphan artifact, no download.
func TestStageCacheOverBudgetSingletonBypasses(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 50)
	var fills atomic.Int32

	_, rel, err := c.Acquire(ctx, "ref", 100, sizedFill(100, &fills))
	if !errors.Is(err, errCacheBypass) {
		t.Fatalf("err=%v, want errCacheBypass", err)
	}
	if rel != nil {
		t.Fatalf("bypass returned a non-nil release")
	}
	if fills.Load() != 0 {
		t.Fatalf("fill ran %d times, want 0 (bypass precedes download)", fills.Load())
	}
}

// TestStageCacheFailingFillReleasesReservation proves a failed fill leaves no
// scratch dir behind and releases its full reservation, so a transient download or
// conversion failure cannot monotonically shrink the effective cap.
func TestStageCacheFailingFillReleasesReservation(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 1<<20)
	boom := errors.New("boom")

	_, _, err := c.Acquire(ctx, "ref", 100, func(_ context.Context, scratch, _ string) error {
		if _, statErr := os.Stat(scratch); statErr != nil {
			t.Errorf("scratch dir missing during fill: %v", statErr)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want boom", err)
	}

	c.mu.Lock()
	reserved, resident := c.reserved, c.resident
	c.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("reserved=%d after failed fill, want 0", reserved)
	}
	if resident != 0 {
		t.Fatalf("resident=%d after failed fill, want 0", resident)
	}

	scratches, _ := filepath.Glob(filepath.Join(c.root, "fill-*"))
	if len(scratches) != 0 {
		t.Fatalf("scratch dirs left behind: %v", scratches)
	}
}

// TestStageCacheCancelledLeaderWaiterStillServed proves a caller cancelled mid-fill
// returns its own context error while the surviving waiter still receives the
// artifact from the detached, decorrelated fill.
func TestStageCacheCancelledLeaderWaiterStillServed(t *testing.T) {
	base := context.Background()
	c := newStageCache(t, 1<<20)

	leaderCtx, cancelLeader := context.WithCancel(base)
	fill, started, proceed := gatedFill(nil, func(dest string) error { return writeArtifact(dest, 100) })

	leaderErr := make(chan error, 1)
	go func() {
		_, _, err := c.Acquire(leaderCtx, "ref", 100, fill)
		leaderErr <- err
	}()
	<-started

	type waiterResult struct {
		path string
		rel  func()
		err  error
	}
	waiterCh := make(chan waiterResult, 1)
	go func() {
		p, r, e := c.Acquire(base, "ref", 100, fill)
		waiterCh <- waiterResult{p, r, e}
	}()

	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err=%v, want context.Canceled", err)
	}
	close(proceed)

	w := <-waiterCh
	if w.err != nil {
		t.Fatalf("waiter err=%v, want the artifact", w.err)
	}
	if _, err := os.Stat(w.path); err != nil {
		t.Fatalf("waiter served a missing artifact: %v", err)
	}
	w.rel()
}

// TestStageCacheReleaseIsIdempotent pins the sync.Once guard on the release: a
// caller that invokes its release twice must not double-decrement the refcount,
// which would let eviction delete an entry a surviving holder still uses.
func TestStageCacheReleaseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 1<<20)

	_, rel, err := c.Acquire(ctx, "ref", 100, sizedFill(100, nil))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()
	rel() // the second release must be a no-op, not a second decrement

	c.mu.Lock()
	e := c.entries["ref"]
	c.mu.Unlock()
	if e == nil || e.refcount != 0 {
		t.Fatalf("refcount=%v, want exactly 0 after a double release", e)
	}
}

// TestStageCacheCancelledLeaderRejoinSingleFill pins the group-lifetime invariant:
// when the sole participant (the leader) cancels while its fill is still running,
// the group must NOT be forgotten, so a fresh caller joins the SAME group and waits
// on the SAME fill rather than starting a second one. Forgetting the group (or
// letting the rejoiner re-trigger the fill) would re-download the object and
// double-count c.resident, silently shrinking the effective cap over time.
func TestStageCacheCancelledLeaderRejoinSingleFill(t *testing.T) {
	base := context.Background()
	c := newStageCache(t, 1<<20)

	leaderCtx, cancelLeader := context.WithCancel(base)
	var fills atomic.Int32
	fill, started, proceed := gatedFill(&fills, func(dest string) error { return writeArtifact(dest, 100) })

	leaderErr := make(chan error, 1)
	go func() {
		_, _, err := c.Acquire(leaderCtx, "ref", 100, fill)
		leaderErr <- err
	}()
	<-started // the leader's fill is now in flight

	// The leader leaves while the fill still runs. The group must survive.
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err=%v, want context.Canceled", err)
	}
	c.mu.Lock()
	_, present := c.groups["ref"]
	c.mu.Unlock()
	if !present {
		t.Fatalf("group forgotten while the fill still runs; a rejoining caller would re-fill")
	}

	// A fresh caller joins the still-running fill and must be served a live artifact.
	type result struct {
		path string
		rel  func()
		err  error
	}
	rejoin := make(chan result, 1)
	go func() {
		p, r, e := c.Acquire(base, "ref", 100, fill)
		rejoin <- result{p, r, e}
	}()
	// Let the rejoiner park on the group, then release the fill.
	close(proceed)

	got := <-rejoin
	if got.err != nil {
		t.Fatalf("rejoiner err=%v, want the artifact", got.err)
	}
	if _, err := os.Stat(got.path); err != nil {
		t.Fatalf("rejoiner served a missing artifact: %v", err)
	}
	if fills.Load() != 1 {
		t.Fatalf("fill ran %d times, want exactly 1 (rejoiner must not re-fill)", fills.Load())
	}

	got.rel()
	c.mu.Lock()
	resident := c.resident
	_, stillGrouped := c.groups["ref"]
	c.mu.Unlock()
	if resident != 100 {
		t.Fatalf("resident=%d, want 100 (the artifact counted once, not doubled)", resident)
	}
	if stillGrouped {
		t.Fatalf("group not forgotten after the last participant left")
	}
	if rc := refcountUnder(t, c, "ref"); rc != 0 {
		t.Fatalf("after the last release refcount=%d, want 0", rc)
	}
}

// TestStageCacheCancelledLeaderRejoinFillError covers the sibling interleaving where
// the shared fill errors while a rejoiner is waiting: the rejoiner must receive the
// error (not a second fill), and the group and its reservation must be cleaned up.
func TestStageCacheCancelledLeaderRejoinFillError(t *testing.T) {
	base := context.Background()
	c := newStageCache(t, 1<<20)
	boom := errors.New("boom")

	leaderCtx, cancelLeader := context.WithCancel(base)
	var fills atomic.Int32
	fill, started, proceed := gatedFill(&fills, func(string) error { return boom })

	leaderErr := make(chan error, 1)
	go func() {
		_, _, err := c.Acquire(leaderCtx, "ref", 100, fill)
		leaderErr <- err
	}()
	<-started
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err=%v, want context.Canceled", err)
	}

	rejoinErr := make(chan error, 1)
	go func() {
		_, _, err := c.Acquire(base, "ref", 100, fill)
		rejoinErr <- err
	}()
	close(proceed)

	if err := <-rejoinErr; !errors.Is(err, boom) {
		t.Fatalf("rejoiner err=%v, want the fill error", err)
	}
	if fills.Load() != 1 {
		t.Fatalf("fill ran %d times, want exactly 1", fills.Load())
	}
	c.mu.Lock()
	reserved, resident := c.reserved, c.resident
	_, stillGrouped := c.groups["ref"]
	c.mu.Unlock()
	if reserved != 0 || resident != 0 {
		t.Fatalf("after a failed fill reserved=%d resident=%d, want 0/0", reserved, resident)
	}
	if stillGrouped {
		t.Fatalf("group not forgotten after a failed fill with no waiters left")
	}
}

// TestStageCacheZeroWaiterBirthDrop covers the settle trigger where the fill
// publishes with no participant left: the leader cancels, no one rejoins, and the
// fill still completes. The birth reference must be dropped (leaving a resident,
// evictable, refcount-0 entry), not stranded -- a stranded birth ref would pin the
// entry forever and shrink the cap.
func TestStageCacheZeroWaiterBirthDrop(t *testing.T) {
	base := context.Background()
	c := newStageCache(t, 1<<20)

	leaderCtx, cancelLeader := context.WithCancel(base)
	fill, started, proceed := gatedFill(nil, func(dest string) error { return writeArtifact(dest, 100) })

	done := make(chan struct{})
	go func() {
		_, _, _ = c.Acquire(leaderCtx, "ref", 100, fill)
		close(done)
	}()
	<-started
	cancelLeader()
	<-done // leader has returned; no one is waiting on the fill

	close(proceed)
	// The fill goroutine finishes asynchronously; poll (bounded) until the group
	// settles, then assert the birth reference was dropped.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		_, grouped := c.groups["ref"]
		e, published := c.entries["ref"]
		var rc int
		if published {
			rc = e.refcount
		}
		resident := c.resident
		c.mu.Unlock()
		if !grouped && published {
			if rc != 0 {
				t.Fatalf("zero-waiter entry refcount=%d, want 0 (birth reference dropped)", rc)
			}
			if resident != 100 {
				t.Fatalf("resident=%d, want 100 (entry cached, birth dropped)", resident)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("group did not settle after the zero-waiter fill (grouped=%v published=%v)", grouped, published)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStageCacheSilentSuccessFillErrors covers stageArtifact's guard against a fill
// that reports success without producing the artifact: it must surface as an acquire
// error (a phantom entry would otherwise be published with a bogus size and later
// served as a deleted file), with the reservation released and the group forgotten.
func TestStageCacheSilentSuccessFillErrors(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 1<<20)

	_, _, err := c.Acquire(ctx, "ref", 100, func(_ context.Context, _, _ string) error {
		return nil // reports success but never writes dest
	})
	if err == nil {
		t.Fatal("expected an error when the fill leaves dest unwritten")
	}

	c.mu.Lock()
	reserved, resident := c.reserved, c.resident
	_, published := c.entries["ref"]
	_, grouped := c.groups["ref"]
	c.mu.Unlock()
	if reserved != 0 || resident != 0 {
		t.Fatalf("reserved=%d resident=%d, want 0/0 after a phantom fill", reserved, resident)
	}
	if published {
		t.Fatal("a phantom entry was published for an unwritten artifact")
	}
	if grouped {
		t.Fatal("group not forgotten after a phantom fill")
	}
}

// TestStageCacheEvictionPressureNeverServesDeletedFile stresses the create-to-attach
// window: many goroutines churn a handful of refs under a cap that forces constant
// eviction, and every served path must point at a readable file. A birth reference
// that failed to hold an entry until its first attach would surface here as a read
// of an evicted (deleted) artifact.
func TestStageCacheEvictionPressureNeverServesDeletedFile(t *testing.T) {
	ctx := context.Background()
	c := newStageCache(t, 300) // ~3 resident entries; 8 refs force churn

	refs := []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7"}
	const size = 100

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				ref := refs[(g+i)%len(refs)]
				path, rel, err := c.Acquire(ctx, ref, size, sizedFill(size, nil))
				if errors.Is(err, errCacheBypass) {
					continue
				}
				if err != nil {
					t.Errorf("acquire %s: %v", ref, err)
					return
				}
				info, statErr := os.Stat(path)
				if statErr != nil {
					t.Errorf("served a deleted artifact for %s: %v", ref, statErr)
					rel()
					return
				}
				if info.Size() != size {
					t.Errorf("artifact for %s is %d bytes, want %d", ref, info.Size(), size)
				}
				rel()
			}
		}(g)
	}
	wg.Wait()
}
