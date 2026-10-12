package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/config"
	"github.com/Scarvis/pet-backend/internal/db"
)

// defaultBaseCurrency is seeded for a freshly registered free-tier organization.
// §8.1 inserts base_currency without an explicit input and §9.1 lists no such env
// var, so the free tier has one fixed currency until a user can choose it.
const defaultBaseCurrency = "USD"

// IssueResult is returned by register/login/refreshToken to the client (§8.1).
type IssueResult struct {
	AccessToken  string        `json:"access_token"`
	RefreshToken string        `json:"refresh_token"`
	ExpiresIn    time.Duration `json:"expires_in"`
}

// Handler serves the authentication Actions (register, login, refreshToken). It
// returns results and errors; cmd/server wires them to HTTP endpoints (§7.1).
type Handler struct {
	pool *pgxpool.Pool
	cfg  *config.Config
	log  *slog.Logger

	key *rsa.PrivateKey // RSA key for signing tokens; loaded lazily and cached on first use
}

// NewHandler builds an auth Handler. The JWT private key is loaded on first token
// issuance (fail-fast if it cannot be read).
func NewHandler(pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{pool: pool, cfg: cfg, log: log}
}

// priv returns the cached RSA signing key, loading and caching it on first call.
func (h *Handler) priv() (*rsa.PrivateKey, error) {
	if h.key != nil {
		return h.key, nil
	}
	priv, err := loadPrivateKey(h.cfg.JWTPrivateKeyPath)
	h.key = priv
	return priv, err
}

// newClaims builds AuthClaims for a freshly issued token pair. Every registered
// user starts as the sole owner of their organization (§8.1).
func (h *Handler) newClaims(userID, orgID string, ttl time.Duration) AuthClaims {
	return AuthClaims{Subject: userID, OrgID: orgID, Roles: []string{"owner"}, TTL: ttl}
}

// randomToken returns a cryptographically random 256-bit refresh-token value.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Register creates an organization + user + owner role + system categories in one
// SERIALIZABLE transaction, then issues a JWT and stores a refresh token (§8.1).
func (h *Handler) Register(ctx context.Context, email, password string) (*IssueResult, error) {
	var orgID, userID uuid.UUID

	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO identity.organizations (plan, base_currency) VALUES ('free', $1) RETURNING id`, defaultBaseCurrency); err != nil {
			return common.Wrap(err)
		}

		hash, err := Hash(password)
		if err != nil {
			return common.Wrap(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity.users (email, password_hash, status) VALUES ($1, $2, 'active') RETURNING id`, email, hash); err != nil {
			return common.Wrap(err)
		}

		var roleID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM identity.roles WHERE name = 'owner' LIMIT 1`).Scan(&roleID); err != nil {
			return common.Wrap(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity.user_roles (user_id, role_id) VALUES ($1, $2)`, userID, roleID); err != nil {
			return common.Wrap(err)
		}

		if err := insertSystemCategories(ctx, tx, orgID); err != nil {
			return common.Wrap(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	refreshToken, err := randomToken()
	if err != nil {
		return nil, err
	}
	refreshHash, err := Hash(refreshToken)
	if err != nil {
		return nil, err
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO identity.refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days')`, userID, refreshHash); err != nil {
		return nil, common.Wrap(err)
	}

	priv, err := h.priv()
	if err != nil {
		return nil, common.Wrap(err)
	}
	claims := h.newClaims(userID.String(), orgID.String(), h.cfg.JWTRefreshTTL)
	accessToken, err := claims.Issue(priv)
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &IssueResult{AccessToken: accessToken, RefreshToken: refreshToken, ExpiresIn: claims.TTL}, nil
}

// Login verifies the password and issues a fresh access + refresh token pair (§8.1).
func (h *Handler) Login(ctx context.Context, email, password string) (*IssueResult, error) {
	var hash, orgID string
	var userID uuid.UUID
	err := h.pool.QueryRow(ctx, `SELECT password_hash, organization_id, id FROM identity.users WHERE email = $1 AND status = 'active'`, email).Scan(&hash, &orgID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, common.NotFound("invalid credentials")
	}
	if err != nil {
		return nil, common.Wrap(err)
	}

	if !Compare(hash, password) {
		return nil, common.BadRequest("invalid credentials")
	}

	refreshToken, err := randomToken()
	if err != nil {
		return nil, err
	}
	refreshHash, err := Hash(refreshToken)
	if err != nil {
		return nil, err
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO identity.refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days')`, userID, refreshHash); err != nil {
		return nil, common.Wrap(err)
	}

	priv, err := h.priv()
	if err != nil {
		return nil, common.Wrap(err)
	}
	claims := h.newClaims(userID.String(), orgID, h.cfg.JWTAccessTTL)
	accessToken, err := claims.Issue(priv)
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &IssueResult{AccessToken: accessToken, RefreshToken: refreshToken, ExpiresIn: claims.TTL}, nil
}

// RefreshToken rotates a valid refresh token into a new access + refresh pair. The
// old token is revoked inside the transaction; the new tokens are issued after it
// commits (§8.1 refreshToken).
func (h *Handler) RefreshToken(ctx context.Context, refreshToken string) (*IssueResult, error) {
	refreshHash, err := Hash(refreshToken)
	if err != nil {
		return nil, common.Wrap(err)
	}

	var userID uuid.UUID
	var expiresAt time.Time
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT user_id, expires_at FROM identity.refresh_tokens WHERE token_hash = $1 AND revoked_at IS NULL`, refreshHash).Scan(&userID, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("invalid refresh token"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if time.Now().After(expiresAt) {
			return common.Wrap(common.BadRequest("refresh token expired"))
		}
		if _, err := tx.Exec(ctx, `UPDATE identity.refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND token_hash = $2`, userID, refreshHash); err != nil {
			return common.Wrap(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var orgID string
	if err := h.pool.QueryRow(ctx, `SELECT organization_id FROM identity.users WHERE id = $1 LIMIT 1`, userID).Scan(&orgID); err != nil {
		return nil, common.Wrap(err)
	}

	priv, err := h.priv()
	if err != nil {
		return nil, common.Wrap(err)
	}
	claims := h.newClaims(userID.String(), orgID, h.cfg.JWTAccessTTL)
	accessToken, err := claims.Issue(priv)
	if err != nil {
		return nil, common.Wrap(err)
	}

	newRefresh, err := randomToken()
	if err != nil {
		return nil, err
	}
	newHash, err := Hash(newRefresh)
	if err != nil {
		return nil, err
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO identity.refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days')`, userID, newHash); err != nil {
		return nil, common.Wrap(err)
	}
	return &IssueResult{AccessToken: accessToken, RefreshToken: newRefresh, ExpiresIn: claims.TTL}, nil
}

// insertSystemCategories seeds the income/expense root categories on registration.
func insertSystemCategories(ctx context.Context, tx pgx.Tx, orgID uuid.UUID) error {
	for _, c := range []struct{ name, kind string }{{"Income", "income"}, {"Expense", "expense"}} {
		if _, err := tx.Exec(ctx, `INSERT INTO tx.categories (org_id, parent_id, name, kind, is_system) VALUES ($1, NULL, $2, $3, true)`, orgID, c.name, c.kind); err != nil {
			return fmt.Errorf("insert system category %q: %w", c.name, err)
		}
	}
	return nil
}
