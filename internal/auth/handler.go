package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
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

// hashRefreshToken hashes a refresh token with SHA-256 for storage and lookup.
// Unlike Hash (bcrypt, salted per password), this is deterministic so the
// refresh_tokens row can be found by token_hash (§8.1).
func hashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
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
		if err := tx.QueryRow(ctx, `INSERT INTO identity.organizations (name, plan, base_currency) VALUES ($1, 'free', $2) RETURNING id`, email, defaultBaseCurrency).Scan(&orgID); err != nil {
			return common.Wrap(err)
		}

		hash, err := Hash(password)
		if err != nil {
			return common.Wrap(err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO identity.users (org_id, email, password_hash) VALUES ($1, $2, $3) RETURNING id`, orgID, email, hash).Scan(&userID); err != nil {
			return common.Wrap(err)
		}

		var roleID int16
		if err := tx.QueryRow(ctx, `SELECT id FROM identity.roles WHERE code = 'owner' LIMIT 1`).Scan(&roleID); err != nil {
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
	refreshHash := hashRefreshToken(refreshToken)
	if _, err := h.pool.Exec(ctx, `INSERT INTO identity.refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days')`, userID, refreshHash); err != nil {
		return nil, common.Wrap(err)
	}

	priv, err := h.priv()
	if err != nil {
		return nil, common.Wrap(err)
	}
	claims := h.newClaims(userID.String(), orgID.String(), h.cfg.JWTAccessTTL)
	accessToken, err := claims.Issue(priv)
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &IssueResult{AccessToken: accessToken, RefreshToken: refreshToken, ExpiresIn: claims.TTL}, nil
}

// Login verifies the password and issues a fresh access + refresh token pair (§8.1).
func (h *Handler) Login(ctx context.Context, email, password string) (*IssueResult, error) {
	var hash string
	var orgID, userID uuid.UUID
	err := h.pool.QueryRow(ctx, `SELECT password_hash, org_id, id FROM identity.users WHERE email = $1 AND status = 'active'`, email).Scan(&hash, &orgID, &userID)
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
	refreshHash := hashRefreshToken(refreshToken)
	if _, err := h.pool.Exec(ctx, `INSERT INTO identity.refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days')`, userID, refreshHash); err != nil {
		return nil, common.Wrap(err)
	}

	priv, err := h.priv()
	if err != nil {
		return nil, common.Wrap(err)
	}
	claims := h.newClaims(userID.String(), orgID.String(), h.cfg.JWTAccessTTL)
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
	refreshHash := hashRefreshToken(refreshToken)

	var userID uuid.UUID
	var expiresAt time.Time
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
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

	var orgID uuid.UUID
	if err := h.pool.QueryRow(ctx, `SELECT org_id FROM identity.users WHERE id = $1 LIMIT 1`, userID).Scan(&orgID); err != nil {
		return nil, common.Wrap(err)
	}

	priv, err := h.priv()
	if err != nil {
		return nil, common.Wrap(err)
	}
	claims := h.newClaims(userID.String(), orgID.String(), h.cfg.JWTAccessTTL)
	accessToken, err := claims.Issue(priv)
	if err != nil {
		return nil, common.Wrap(err)
	}

	newRefresh, err := randomToken()
	if err != nil {
		return nil, err
	}
	newHash := hashRefreshToken(newRefresh)
	if _, err := h.pool.Exec(ctx, `INSERT INTO identity.refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days')`, userID, newHash); err != nil {
		return nil, common.Wrap(err)
	}
	return &IssueResult{AccessToken: accessToken, RefreshToken: newRefresh, ExpiresIn: claims.TTL}, nil
}

// insertSystemCategories seeds a new organization's category tree by copying
// the global tx.system_categories template (migration 000009): roots first,
// then children remapped onto the freshly inserted org roots. When the
// template table is empty (e.g. migration 000009 not applied yet), it falls
// back to the built-in Russian default set (27 rows total, well under the
// 100-per-org cap enforced by tx.enforce_category_limit).
func insertSystemCategories(ctx context.Context, tx pgx.Tx, orgID uuid.UUID) error {
	type sysCat struct {
		id       uuid.UUID
		parentID *uuid.UUID
		name     string
		kind     string
		icon     *string
	}
	rows, err := tx.Query(ctx, `SELECT id, parent_id, name, kind, icon FROM tx.system_categories ORDER BY parent_id NULLS FIRST, name`)
	if err != nil {
		return fmt.Errorf("read system categories: %w", err)
	}
	var template []sysCat
	for rows.Next() {
		var c sysCat
		if err := rows.Scan(&c.id, &c.parentID, &c.name, &c.kind, &c.icon); err != nil {
			rows.Close()
			return fmt.Errorf("scan system category: %w", err)
		}
		template = append(template, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read system categories: %w", err)
	}

	if len(template) == 0 {
		return insertDefaultCategories(ctx, tx, orgID)
	}

	// Single-level template (roots, then children), so one ordered pass
	// suffices: roots have parent_id NULL and sort first.
	remapped := make(map[uuid.UUID]uuid.UUID, len(template))
	for _, c := range template {
		var parent any
		if c.parentID != nil {
			newParent, ok := remapped[*c.parentID]
			if !ok {
				return fmt.Errorf("system category %q references unknown parent", c.name)
			}
			parent = newParent
		} else {
			parent = nil
		}
		var newID uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO tx.categories (org_id, parent_id, name, kind, icon) VALUES ($1, $2, $3, $4, $5) RETURNING id`, orgID, parent, c.name, c.kind, c.icon).Scan(&newID); err != nil {
			return fmt.Errorf("insert category %q: %w", c.name, err)
		}
		remapped[c.id] = newID
	}
	return nil
}

// insertDefaultCategories seeds the built-in Russian category set (roots
// Доходы/Расходы + 25 children). Used only when tx.system_categories is
// empty; normally the template table carries this same set (migration 000009).
func insertDefaultCategories(ctx context.Context, tx pgx.Tx, orgID uuid.UUID) error {
	incomeChildren := []string{
		"Зарплата", "Фриланс", "Бизнес", "Инвестиции", "Аренда", "Подарки", "Прочее",
	}
	expenseChildren := []string{
		"Продукты", "Кафе и рестораны", "Транспорт", "Жильё",
		"Коммунальные услуги", "Связь и интернет", "Здоровье",
		"Одежда и обувь", "Образование", "Развлечения", "Спорт",
		"Дети", "Питомцы", "Путешествия", "Красота", "Подписки",
		"Налоги", "Прочее",
	}
	roots := []struct {
		name     string
		kind     string
		children []string
	}{
		{"Доходы", "income", incomeChildren},
		{"Расходы", "expense", expenseChildren},
	}
	for _, r := range roots {
		var rootID uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO tx.categories (org_id, parent_id, name, kind) VALUES ($1, NULL, $2, $3) RETURNING id`, orgID, r.name, r.kind).Scan(&rootID); err != nil {
			return fmt.Errorf("insert system category %q: %w", r.name, err)
		}
		for _, child := range r.children {
			if _, err := tx.Exec(ctx, `INSERT INTO tx.categories (org_id, parent_id, name, kind) VALUES ($1, $2, $3, $4)`, orgID, rootID, child, r.kind); err != nil {
				return fmt.Errorf("insert system category %q: %w", child, err)
			}
		}
	}
	return nil
}
