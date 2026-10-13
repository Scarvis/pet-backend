package common

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// userIDCtxKey stores the acting user for an Action handler. Hasura forwards the
// authenticated subject as the X-Hasura-User-ID header; cmd/server parses it and
// puts it into request context before dispatching (§7.1). Tests inject it directly.
type userIDCtxKey int

const userIDCtxKeyVal userIDCtxKey = 0

// withUserID returns a copy of ctx carrying id as the acting user.
func withUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userIDCtxKeyVal, id)
}

// WithUserID returns a copy of ctx carrying id as the acting user. It is the
// exported wrapper cmd/server uses to inject the Hasura-forwarded
// X-Hasura-User-ID into request context before dispatching (§7.1).
func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return withUserID(ctx, id)
}

// CurrentUser returns the acting user for an Action handler, or NotFound when no
// current user is set in context (§7.1). The returned *Error renders as HTTP 404 so
// a missing identity surfaces cleanly through the router and panic recovery.
func CurrentUser(ctx context.Context) (uuid.UUID, error) {
	id, ok := ctx.Value(userIDCtxKeyVal).(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, NotFound("missing current user")
	}
	return id, nil
}

// ActingOrg returns the organization owned by the acting user (§7.1). It resolves
// org_id from identity.users so ownership checks and inserts are authoritative even
// when only X-Hasura-User-ID was forwarded. pool is a *pgxpool.Pool; its connection
// is acquired for the duration of the call and released afterwards.
func ActingOrg(ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, error) {
	userID, err := CurrentUser(ctx)
	if err != nil {
		return uuid.Nil, err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return uuid.Nil, Wrap(fmt.Errorf("acquire connection: %w", err))
	}
	defer conn.Release()

	var orgID uuid.UUID
	err = conn.QueryRow(ctx, `SELECT org_id FROM identity.users WHERE id = $1`, userID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, NotFound("user has no organization")
	}
	if err != nil {
		return uuid.Nil, Wrap(err)
	}
	return orgID, nil
}

// ValidCurrency reports whether code exists in fx.currencies. Business logic fails
// fast with a 400 (common.BadRequest) instead of letting the foreign key reject the
// insert as an opaque error (§8.2). pool is a *pgxpool.Pool; its connection is
// acquired for the duration of the call and released afterwards.
func ValidCurrency(ctx context.Context, pool *pgxpool.Pool, code string) bool {
	if code == "" {
		return false
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false
	}
	defer conn.Release()

	var exists bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fx.currencies WHERE code = $1)`, code).Scan(&exists)
	return err == nil && exists
}
