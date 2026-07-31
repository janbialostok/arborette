package sandbox

import (
	"context"
	"sync"
)

// ClassDefault is the single request class registered today, covering /introspect,
// /execute, and /document/text. Later request kinds (an analyze class, say) register
// their own class name and slot count without reshaping the limiter, so nothing
// here hard-codes the default where a class parameter can flow.
const ClassDefault = "default"

// classLimiter bounds concurrent in-flight work per request class. Each class is a
// buffered-channel semaphore sized at construction; Acquire blocks until a slot
// frees or the caller's ctx is done. Blocking is a backstop: internal callers size
// their own worker pools within the cap, so the limiter is the ceiling, not the
// steady-state gate (the rejected alternative -- a fast 503 -- would push retry
// loops into every internal client).
type classLimiter struct {
	slots map[string]chan struct{}
}

// NewClassLimiter builds a limiter from a class→capacity map. A capacity below 1 is
// clamped to 1 so a misconfigured knob throttles rather than deadlocks a class.
func NewClassLimiter(capacities map[string]int) *classLimiter {
	slots := make(map[string]chan struct{}, len(capacities))
	for class, n := range capacities {
		if n < 1 {
			n = 1
		}
		slots[class] = make(chan struct{}, n)
	}
	return &classLimiter{slots: slots}
}

// Acquire takes a slot for class, blocking until one frees or ctx is done. It
// returns an idempotent release that returns the slot. An unregistered class is
// unbounded (a no-op release), not blocked, so a new route without its own class
// runs rather than stalling. The error is non-nil only when ctx was cancelled while
// waiting.
func (l *classLimiter) Acquire(ctx context.Context, class string) (func(), error) {
	sem, ok := l.slots[class]
	if !ok {
		return func() {}, nil
	}
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
