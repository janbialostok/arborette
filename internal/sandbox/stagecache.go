package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// errCacheBypass is returned by Acquire when the requested worst-case reservation
// cannot fit even after evicting every unreferenced entry: the caller falls back
// to per-request staging. It is a control signal, never surfaced to an HTTP
// client, so it carries no status mapping.
var errCacheBypass = errors.New("stage cache: reservation does not fit")

// fillTimeout is the absolute deadline the shared fill runs under. It is derived
// from context.WithoutCancel so no single caller's cancellation aborts a fill the
// other waiters depend on; the timeout bounds a fill whose every waiter has gone.
// Generous by design: a fill stages an object up to maxObjectBytes and, for CSV,
// converts it to Parquet, both of which can be slow for a large source.
const fillTimeout = 10 * time.Minute

// FillFunc downloads (and, for CSV, converts) a data source into dest, using
// scratch as a workspace and ctx as its deadline. scratch is a per-fill subdir
// under the cache root, removed on every exit path; dest is the extension-less
// cache-owned artifact path the cache stats, serves, and evicts. Both are covered
// by the fill's reservation.
type FillFunc func(ctx context.Context, scratch, dest string) error

// entry is one cached artifact: its on-disk path, settled size, live refcount,
// and LRU stamp. An entry is evictable only at refcount 0; while an entry has any
// holder it is never deleted, so a query can never lose the file out from under it.
type entry struct {
	path     string
	size     int64
	refcount int
	lru      uint64
}

// fillGroup coordinates one ref's cold fill and the concurrent Acquires that share
// it. The leader spawns the fill in a detached goroutine and then, like every
// waiter, blocks on done; done is closed once the fill has published an entry,
// errored, or bypassed, at which point entry/err carry the outcome. pending counts
// participants that have not yet attached their own refcount or left.
//
// The group lives in StageCache.groups for exactly one fill and is removed only once
// the fill is finished AND pending reaches 0 -- so the group's lifetime and the
// fill's are one and the same. This is the load-bearing property: a late caller
// either hits the published entry or joins *this* group and waits on its done, and
// can never start a second fill against a group whose fill has vanished. Holding the
// birth reference (the published entry's refcount 1) until the last participant
// attaches also stops a fast holder releasing before a slow waiter attaches, which
// would drop the entry to refcount 0 and let eviction delete the file mid-handoff.
//
// Every field is guarded by StageCache.mu; done is closed once by the fill goroutine
// after its final locked section (entry/err/finished are all set before the close,
// so a waiter reading them under mu after receiving done sees settled values).
type fillGroup struct {
	done     chan struct{}
	entry    *entry
	err      error
	pending  int
	finished bool
	settled  bool
}

// StageCache is a bounded, boot-scoped, content-addressed staging cache over the
// sandbox's immutable object-store refs. It is a pure performance layer: it never
// changes what a read returns, only whether the download and parse are re-paid.
//
// The load-bearing disk invariant is resident bytes + in-flight reservations <=
// maxBytes, always. A reservation is charged once per fill by the group's leader
// (never once per calling Acquire), so N concurrent same-ref cold requests collapse
// to one fill charging one reservation rather than N; a reservation that cannot fit
// even after evicting every unreferenced entry bypasses before any download, so no
// cache-dir orphan is written and no object is downloaded twice for one request.
type StageCache struct {
	root     string
	maxBytes int64

	mu       sync.Mutex
	entries  map[string]*entry
	groups   map[string]*fillGroup
	resident int64
	reserved int64
	clock    uint64

	fills atomic.Int64
	hits  atomic.Int64
}

// NewStageCache builds a cache rooted at root with a maxBytes disk budget. root is
// created if absent; the caller (cmd/sandbox) is responsible for emptying a
// configured directory at boot so no prior-boot file sits on disk uncounted.
func NewStageCache(root string, maxBytes int64) (*StageCache, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create stage cache root: %w", err)
	}
	return &StageCache{
		root:     root,
		maxBytes: maxBytes,
		entries:  map[string]*entry{},
		groups:   map[string]*fillGroup{},
	}, nil
}

// Acquire returns a staged path for ref plus a release the caller defers. A hit
// takes a fresh refcount and returns immediately. A miss joins (or creates) the ref's
// fill group: the first caller becomes the leader and spawns the shared fill detached
// from any caller's ctx; every caller then blocks on the group's completion versus
// its own ctx, so one caller's cancellation returns only its own error while the fill
// serves the rest. An errCacheBypass return means the reservation did not fit; the
// caller stages into its own temp dir instead.
//
// Each calling Acquire takes its own refcount and gets its own independent release,
// so N concurrent first-requesters yield one fill, N refcounts, and N releases, and
// the entry survives eviction pressure until the last release.
func (c *StageCache) Acquire(ctx context.Context, ref string, reserve int64, fill FillFunc) (string, func(), error) {
	c.mu.Lock()
	if e, ok := c.entries[ref]; ok {
		e.refcount++
		e.lru = c.nextStampLocked()
		c.mu.Unlock()
		c.hits.Add(1)
		return e.path, c.releaseOnce(e), nil
	}
	g, ok := c.groups[ref]
	if !ok {
		g = &fillGroup{done: make(chan struct{})}
		c.groups[ref] = g
		g.pending++
		c.mu.Unlock()
		// Leader: run the shared fill detached from this caller's ctx, then attach as
		// an ordinary participant so its refcount is accounted like every waiter's.
		go c.runFill(ctx, ref, reserve, fill, g)
		return c.attach(ctx, ref, g)
	}
	g.pending++
	c.mu.Unlock()
	return c.attach(ctx, ref, g)
}

// attach blocks a participant until the group's fill completes or its own ctx is
// done, then accounts its departure. On completion it takes its own refcount off the
// published entry (increment before settle, so the birth reference is dropped only
// after this holder is counted); on cancellation it leaves without a refcount.
func (c *StageCache) attach(ctx context.Context, ref string, g *fillGroup) (string, func(), error) {
	select {
	case <-ctx.Done():
		c.mu.Lock()
		g.pending--
		c.settleGroupLocked(ref, g)
		c.mu.Unlock()
		return "", nil, ctx.Err()
	case <-g.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		g.pending--
		if g.err != nil {
			c.settleGroupLocked(ref, g)
			return "", nil, g.err
		}
		e := g.entry
		e.refcount++
		e.lru = c.nextStampLocked()
		c.settleGroupLocked(ref, g)
		return e.path, c.releaseOnce(e), nil
	}
}

// runFill reserves, stages, and publishes exactly once for a cold ref, as the
// group's detached leader goroutine. The reservation is charged before any download
// and drained on every exit -- settled to the artifact's actual size on success,
// released in full on admission bypass or staging error -- so a failed fill can
// never monotonically shrink the effective cap. It records the outcome on the group
// and closes done exactly once so every waiter (and the leader itself) is released.
func (c *StageCache) runFill(reqCtx context.Context, ref string, reserve int64, fill FillFunc, g *fillGroup) {
	c.mu.Lock()
	if !c.admitLocked(reserve) {
		c.finishGroupLocked(ref, g, nil, errCacheBypass)
		c.mu.Unlock()
		close(g.done)
		return
	}
	c.reserved += reserve
	c.mu.Unlock()

	c.fills.Add(1)

	// Detach from the leader's request context so a cancelled or timed-out leader
	// does not abort the fill the other waiters are blocked on; bound it absolutely
	// so a fill with no remaining waiter still terminates.
	fillCtx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), fillTimeout)
	defer cancel()

	dest, size, err := c.stageArtifact(fillCtx, ref, fill)

	c.mu.Lock()
	c.reserved -= reserve
	var e *entry
	if err == nil {
		e = &entry{path: dest, size: size, refcount: 1, lru: c.nextStampLocked()}
		c.entries[ref] = e
		c.resident += e.size
	}
	c.finishGroupLocked(ref, g, e, err)
	c.mu.Unlock()
	close(g.done)
}

// stageArtifact downloads (and, for CSV, converts) the ref into the cache-owned
// artifact, using a per-fill scratch subdir removed on every exit. It runs without
// the lock (its I/O is the slow part); the caller publishes the result under the
// lock. A fill that reports success without writing dest surfaces as a stat error.
func (c *StageCache) stageArtifact(ctx context.Context, ref string, fill FillFunc) (dest string, size int64, err error) {
	scratch, err := os.MkdirTemp(c.root, "fill-")
	if err != nil {
		return "", 0, fmt.Errorf("create fill scratch: %w", err)
	}
	defer os.RemoveAll(scratch)

	dest = filepath.Join(c.root, hashRef(ref))
	if err := fill(ctx, scratch, dest); err != nil {
		return "", 0, err
	}
	info, err := os.Stat(dest)
	if err != nil {
		os.Remove(dest)
		return "", 0, fmt.Errorf("stat staged artifact: %w", err)
	}
	return dest, info.Size(), nil
}

// finishGroupLocked records the fill's outcome and settles the group if no
// participant remains. On success e is the published entry holding its birth
// reference and err is nil; on bypass or staging error e is nil and err carries the
// reason.
func (c *StageCache) finishGroupLocked(ref string, g *fillGroup, e *entry, err error) {
	g.finished = true
	g.entry = e
	g.err = err
	c.settleGroupLocked(ref, g)
}

// settleGroupLocked drops the birth reference and forgets the group once the fill
// has finished and its last participant has left. Refusing to settle while pending
// remains holds the birth reference until the last waiter attaches (so the entry is
// never evictable mid-handoff) and keeps the group resident so a late caller joins
// this same group rather than starting a second fill.
func (c *StageCache) settleGroupLocked(ref string, g *fillGroup) {
	if g.settled || !g.finished || g.pending > 0 {
		return
	}
	g.settled = true
	if g.entry != nil {
		g.entry.refcount--
	}
	if c.groups[ref] == g {
		delete(c.groups, ref)
	}
}

// admitLocked evicts unreferenced entries in LRU order until resident + reserved +
// reserve fits under maxBytes, reporting whether it fits. A reservation larger than
// the whole budget can never fit even in an empty cache, so it bypasses up front
// without discarding warm entries it could not use anyway (a misconfiguration where
// StageCacheMaxBytes < a CSV's 2x reserve would otherwise nuke the cache per request).
func (c *StageCache) admitLocked(reserve int64) bool {
	if reserve > c.maxBytes {
		return false
	}
	for c.resident+c.reserved+reserve > c.maxBytes {
		if !c.evictOneLocked() {
			return false
		}
	}
	return true
}

// evictOneLocked deletes the least-recently-used refcount-0 entry, returning false
// when none is evictable (every resident entry still has a holder).
func (c *StageCache) evictOneLocked() bool {
	var victim string
	var victimEntry *entry
	for ref, e := range c.entries {
		if e.refcount != 0 {
			continue
		}
		if victimEntry == nil || e.lru < victimEntry.lru {
			victim, victimEntry = ref, e
		}
	}
	if victimEntry == nil {
		return false
	}
	os.Remove(victimEntry.path)
	delete(c.entries, victim)
	c.resident -= victimEntry.size
	return true
}

// releaseOnce returns an idempotent release for a specific entry. It decrements
// that entry's refcount once regardless of how many times the caller invokes it;
// an entry dropping to refcount 0 stays resident (its whole point is amortization)
// and is evicted lazily only when a later reservation needs the room.
func (c *StageCache) releaseOnce(e *entry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			e.refcount--
			c.mu.Unlock()
		})
	}
}

func (c *StageCache) nextStampLocked() uint64 {
	c.clock++
	return c.clock
}

// Fills and Hits report the download and cache-hit counts. They exist for the
// integration test that proves a data source is downloaded exactly once across a
// run's many measurements (objectstore has no Delete, so a counter is the only
// available observable that a second call did not re-download).
func (c *StageCache) Fills() int64 { return c.fills.Load() }
func (c *StageCache) Hits() int64  { return c.hits.Load() }

// hashRef maps an object-store ref (a key with slashes and arbitrary bytes) to a
// safe, fixed-width, extension-less filename. It is the one name the cache stats,
// serves, and evicts, so the reader dispatch keys on the staging path, not on it.
func hashRef(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])
}
