package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// Pool is a pgxpool wrapper that registers the pgvector type on every
// connection. Without that registration pgx cannot encode/decode the vector
// type and the embedding path fails at runtime despite compiling. Because
// RegisterTypes looks up the vector type's OID, building a Pool fails (taking
// the whole pool down) unless migration 0001 has already created the extension
// -- so construct a Pool only after Migrate has applied.
type Pool struct {
	*pgxpool.Pool
}

// NewPool opens a registered pgxpool against the given runtime DSN.
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping pool: %w", err)
	}
	return &Pool{Pool: pool}, nil
}
