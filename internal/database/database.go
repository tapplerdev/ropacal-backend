package database

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// Connection-pool defaults. Overridable via env (see configurePool).
//
// database/sql's defaults are unlimited open connections — fine while every
// statement was a bare pool call, but the orgdb tenancy wrapper runs each
// statement inside its own BEGIN/set_config/COMMIT, which holds connections
// slightly longer and makes an explicit ceiling important: Railway Postgres
// ships max_connections ≈ 100 with no PgBouncer in front, and this process
// owns the only pool against it (verified: database.Connect is the sole
// sqlx.Connect/sql.Open site; workers, handlers and proxies all share it).
// 20 open conns is generous for today's traffic while leaving ample headroom
// for redeploy overlap and support psql sessions.
const (
	defaultMaxOpenConns    = 20
	defaultMaxIdleConns    = 10
	defaultConnMaxLifetime = 30 * time.Minute
	defaultConnMaxIdleTime = 5 * time.Minute
)

// envInt reads an integer env var, falling back to def on absence or garbage
// (garbage warns — a typo'd limit must not silently become unlimited).
func envInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("⚠️  %s=%q is not an integer — using default %d", key, raw, def)
		return def
	}
	return n
}

// envDuration reads a Go duration env var (e.g. "30m", "1h"), falling back
// to def on absence or garbage.
func envDuration(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("⚠️  %s=%q is not a duration (try \"30m\") — using default %s", key, raw, def)
		return def
	}
	return d
}

// configurePool applies the connection-pool limits, env-tunable via
// DB_MAX_OPEN_CONNS, DB_MAX_IDLE_CONNS, DB_CONN_MAX_LIFETIME and
// DB_CONN_MAX_IDLE_TIME.
func configurePool(db *sqlx.DB) {
	maxOpen := envInt("DB_MAX_OPEN_CONNS", defaultMaxOpenConns)
	maxIdle := envInt("DB_MAX_IDLE_CONNS", defaultMaxIdleConns)
	maxLifetime := envDuration("DB_CONN_MAX_LIFETIME", defaultConnMaxLifetime)
	maxIdleTime := envDuration("DB_CONN_MAX_IDLE_TIME", defaultConnMaxIdleTime)

	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(maxLifetime)
	db.SetConnMaxIdleTime(maxIdleTime)

	log.Printf("🏊 DB pool: max_open=%d max_idle=%d conn_max_lifetime=%s conn_max_idle_time=%s",
		maxOpen, maxIdle, maxLifetime, maxIdleTime)
}

func Connect(dbURL string) (*sqlx.DB, error) {
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Println("🔌 DATABASE CONNECTION ATTEMPT")
	log.Printf("   📍 Database URL length: %d characters", len(dbURL))
	log.Printf("   📍 URL prefix: %s...", dbURL[:min(30, len(dbURL))])
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	log.Println("🔄 Step 1: Attempting sqlx.Connect()...")
	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ DATABASE CONNECTION FAILED AT sqlx.Connect()")
		log.Printf("   Error type: %T", err)
		log.Printf("   Error message: %v", err)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	log.Println("✅ Step 1 Complete: sqlx.Connect() succeeded")

	log.Println("🔄 Step 2: Testing connection with Ping()...")
	if err := db.Ping(); err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ DATABASE CONNECTION FAILED AT Ping()")
		log.Printf("   Error type: %T", err)
		log.Printf("   Error message: %v", err)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	log.Println("✅ Step 2 Complete: Ping() succeeded")

	configurePool(db)

	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Println("✅ DATABASE CONNECTION SUCCESSFUL")
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return db, nil
}

// Migrate() WAS HERE AND IS DELIBERATELY GONE (2026-08-02, Track A3).
//
// It ran ~223 idempotent DDL statements at every boot — 29 CREATE TABLE, 108
// ALTER TABLE, 100 CREATE INDEX — none of which carried RLS. Because both it
// and goose used CREATE TABLE IF NOT EXISTS, whichever ran first won, and goose
// won only by ordering luck. The day it didn't, a table holding tenant data
// came up with no organization_id and no policy, and nothing said so. That is
// exactly how shift_bins was resurrected untenanted in Feb 2026.
//
// Proven redundant before removal, not assumed: a database built from goose
// ALONE was diffed against production across every catalog dimension —
// 721 columns, 247 indexes, 199 constraints, 1 trigger, 40 policies — and
// nothing existed in production that goose does not create. The only deltas
// were goose_db_version's own objects and a constraint added the same day.
//
// The tail of the function (single-tenant warehouse_location seed) went with
// it: it was already guarded on `organizations` NOT existing, and goose creates
// organizations in 00001, so the guard could never pass again.
//
// DO NOT ADD BOOT-TIME DDL HERE. Schema changes go in a numbered goose
// migration, where they are versioned, ordered, and reviewable.
