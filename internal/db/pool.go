// Package db owns the PostgreSQL connection pool and the transaction helpers
// used by every mutating handler (see ARCHITECTURE.md §7.1).
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/config"
)

const (
	poolMaxConns    = 20
	poolMinConns    = 2
	poolHealthCheck = 10 * time.Second
)

// NewPool builds a pgx connection pool from DATABASE_URL and verifies the
// database is reachable before returning. Callers should keep the returned pool
// for the lifetime of the process.
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	pgCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pgCfg.MaxConns = poolMaxConns
	pgCfg.MinConns = poolMinConns
	pgCfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, pgCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := ping(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	slog.Info("connected to database", "max_conns", poolMaxConns)
	return pool, nil
}

// ping opens a short-lived connection and confirms the server is up.
func ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, poolHealthCheck)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	return nil
}
