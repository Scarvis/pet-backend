// Package config loads environment variables and command-line flags into a
// typed configuration, validates it, and exposes it to the rest of the service.
package config

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// Config holds every setting the Go business-logic service needs at startup.
type Config struct {
	ListenAddr         string        // address the HTTP/Action server listens on (§9.1 LISTEN_ADDR)
	DatabaseURL        string        // PostgreSQL connection string (DATABASE_URL)
	JWTPrivateKeyPath  string        // path to the RS256 signing key PEM (JWT_PRIVATE_KEY_PATH)
	JWTAccessTTL       time.Duration // access-token lifetime (JWT_ACCESS_TTL, default 15m)
	JWTRefreshTTL      time.Duration // refresh-token lifetime (JWT_REFRESH_TTL, default 720h)
	FXAPIURL           string        // external FX rates API base URL (§9.1 FX_API_URL)
	FXAPIKey           string        // key for the FX rates API (§9.1 FX_API_KEY)
	StorageBackend     string        // import storage backend: "local" or "s3" (§9.1 STORAGE_BACKEND)
	HasuraActionSecret string        // shared secret Hasura sends as X-Action-Secret (§9.1, §11)
}

// Load reads configuration from the environment and optional flags, validates it,
// and returns a ready-to-use Config. Flags override environment variables of the
// same name when both are set.
func Load() (*Config, error) {
	fs := flag.NewFlagSet("financeapp", flag.ContinueOnError)

	listenAddr := fs.String("listen-addr", envOr("LISTEN_ADDR", ":9090"), "address to listen on")
	databaseURL := fs.String("database-url", envOr("DATABASE_URL", ""), "PostgreSQL connection string")
	jwtKeyPath := fs.String("jwt-private-key-path", envOr("JWT_PRIVATE_KEY_PATH", ""), "path to the RS256 private key PEM")
	jwtAccessTTL := fs.Duration("jwt-access-ttl", parseDuration(envOr("JWT_ACCESS_TTL", "15m")), "access token lifetime")
	jwtRefreshTTL := fs.Duration("jwt-refresh-ttl", parseDuration(envOr("JWT_REFRESH_TTL", "720h")), "refresh token lifetime")
	fxAPIURL := fs.String("fx-api-url", envOr("FX_API_URL", "https://api.exchangerate.host"), "external FX rates API base URL")
	fxAPIKey := fs.String("fx-api-key", envOr("FX_API_KEY", ""), "key for the FX rates API")
	storageBackend := fs.String("storage-backend", envOr("STORAGE_BACKEND", "local"), `import storage backend: "local" or "s3"`)
	actionSecret := fs.String("hasura-action-secret", envOr("HASURA_ACTION_SECRET", ""), "shared secret Hasura sends as X-Action-Secret")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}

	cfg := &Config{
		ListenAddr:         *listenAddr,
		DatabaseURL:        *databaseURL,
		JWTPrivateKeyPath:  *jwtKeyPath,
		JWTAccessTTL:       *jwtAccessTTL,
		JWTRefreshTTL:      *jwtRefreshTTL,
		FXAPIURL:           *fxAPIURL,
		FXAPIKey:           *fxAPIKey,
		StorageBackend:     *storageBackend,
		HasuraActionSecret: *actionSecret,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the mandatory fields and the storage-backend enum.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	switch c.StorageBackend {
	case "local", "s3":
	default:
		return fmt.Errorf("STORAGE_BACKEND must be \"local\" or \"s3\", got %q", c.StorageBackend)
	}
	if c.JWTPrivateKeyPath == "" {
		return fmt.Errorf("JWT_PRIVATE_KEY_PATH is required for RS256 signing")
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(fmt.Sprintf("invalid duration %q: %v", s, err))
	}
	return d
}
