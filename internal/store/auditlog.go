package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// AuditRecord is one append-only audit entry.
type AuditRecord struct {
	Actor     string
	Action    string
	EventType string
	Detail    map[string]any
}

// AuditLog is an insert-only audit-trail API. It exposes no update or delete
// method: the boundary is enforced primarily by the least-privilege
// arborette_orchestrator role (which lacks UPDATE/DELETE on audit_log), with
// this insert-only type as the second layer.
type AuditLog struct {
	pool *Pool
}

// NewAuditLog wires the audit log to a pool.
func NewAuditLog(pool *Pool) *AuditLog {
	return &AuditLog{pool: pool}
}

// Append inserts one audit record.
func (a *AuditLog) Append(ctx context.Context, record AuditRecord) error {
	var detail []byte
	if record.Detail != nil {
		b, err := json.Marshal(record.Detail)
		if err != nil {
			return fmt.Errorf("marshal audit detail: %w", err)
		}
		detail = b
	}
	_, err := a.pool.Exec(ctx,
		"INSERT INTO audit_log (actor, action, event_type, detail) VALUES ($1, $2, $3, $4)",
		record.Actor, record.Action, record.EventType, detail,
	)
	if err != nil {
		return fmt.Errorf("append audit record: %w", err)
	}
	return nil
}
