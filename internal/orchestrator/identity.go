package orchestrator

import "context"

// Analyst is the acting identity an audit record is stamped with.
type Analyst struct {
	ID string
}

// Identity resolves the current analyst for the audit-write path. It is the
// single seam a future SSO/per-department implementation replaces; the rest of
// the service depends on this interface, not on any auth mechanism.
type Identity interface {
	Current(ctx context.Context) (Analyst, error)
}

// StubIdentity returns one configured analyst id for every request, backing the
// deferred full-auth work without threading auth through call sites. It remains
// the identity behind the /internal/audit path and boot-time reconciliation,
// which the session guard never touches.
type StubIdentity struct {
	ID string
}

// Current returns the configured stub analyst.
func (s StubIdentity) Current(context.Context) (Analyst, error) {
	return Analyst{ID: s.ID}, nil
}

// SessionIdentity resolves the acting analyst from the session the guard
// stashed in the request context, falling back to its Fallback identity for
// paths that never passed the guard (the /internal/audit HTTP seam and boot
// reconciliation). Human actions through guarded routes are therefore audit-
// stamped with the real user; service and boot writes keep the stub.
type SessionIdentity struct {
	Fallback Identity
}

// Current returns the session's analyst when one is in flight, otherwise the
// fallback identity.
func (s SessionIdentity) Current(ctx context.Context) (Analyst, error) {
	if a, ok := AnalystsFromContext(ctx); ok {
		return a, nil
	}
	return s.Fallback.Current(ctx)
}
