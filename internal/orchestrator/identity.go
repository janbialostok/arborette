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
// deferred full-auth work without threading auth through call sites.
type StubIdentity struct {
	ID string
}

// Current returns the configured stub analyst.
func (s StubIdentity) Current(context.Context) (Analyst, error) {
	return Analyst{ID: s.ID}, nil
}
