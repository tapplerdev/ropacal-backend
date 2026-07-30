package database

import (
	"embed"
	"fmt"
	"log"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"
)

// Versioned schema migrations, embedded so the binary carries its own schema and
// a deploy needs no separate migration step.
//
// The archive/ subdirectory is deliberately NOT embedded: those are the SQL
// files that were applied by hand before goose existed. Their effects are all
// captured in the baseline, so re-running them would be wrong; they are kept
// only as history.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationDir = "migrations"

// RunMigrations brings the database to the latest schema version.
//
// WHY THIS EXISTS. The schema used to live in two places that disagreed: an
// idempotent DDL list re-run on every boot (Migrate, below) and hand-applied
// migrations/*.sql files with nothing recording that they had run. Production
// was correct by accretion, but the repository could not rebuild it — verified
// by pointing the server at an empty database, where boot ABORTED and 24 of the
// 223 statements failed across 4 tables. route_tasks, the central table of the
// itinerary domain, had no CREATE TABLE anywhere in the repo at all.
//
// goose gives what was missing: an ordered, versioned schema with a record in
// the database of exactly which versions have been applied.
func RunMigrations(db *sqlx.DB) error {
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger()) // goose logs per-statement; too noisy at boot

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}

	if err := baselineIfNeeded(db); err != nil {
		return err
	}

	before, err := goose.GetDBVersion(db.DB)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if err := goose.Up(db.DB, migrationDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	after, err := goose.GetDBVersion(db.DB)
	if err != nil {
		return fmt.Errorf("read schema version after migrate: %w", err)
	}
	if after == before {
		log.Printf("🗄️  [Schema] up to date at version %d", after)
	} else {
		log.Printf("🗄️  [Schema] migrated %d → %d", before, after)
	}
	return nil
}

// baselineIfNeeded marks the baseline as already-applied on a database that
// ALREADY has the schema, instead of trying to run it.
//
// This is the whole reason adopting goose against a live system is delicate.
// Production already contains every object in 00001_baseline_schema.sql, so
// `goose up` there would fail on the first CREATE TABLE — and a failed boot on
// the migration path takes the service down. Detection is deliberately based on
// a table that has existed for years (`users`), not on an absence of the goose
// version table: a fresh database has neither, an existing one has users but no
// goose table, and that is exactly the case that needs stamping rather than
// running.
//
// A fresh database falls through untouched and goose runs the baseline normally.
func baselineIfNeeded(db *sqlx.DB) error {
	var gooseTableExists bool
	if err := db.Get(&gooseTableExists,
		`SELECT to_regclass('public.goose_db_version') IS NOT NULL`); err != nil {
		return fmt.Errorf("probe goose version table: %w", err)
	}
	if gooseTableExists {
		return nil // already under goose management
	}

	var schemaExists bool
	if err := db.Get(&schemaExists,
		`SELECT to_regclass('public.users') IS NOT NULL`); err != nil {
		return fmt.Errorf("probe existing schema: %w", err)
	}
	if !schemaExists {
		return nil // genuinely fresh — let goose build it
	}

	log.Println("🗄️  [Schema] existing database detected with no goose history — baselining at version 1")
	log.Println("   (the schema is already present; the baseline is being STAMPED, not executed)")

	// goose creates its version table lazily; EnsureDBVersion does it without
	// running anything.
	if _, err := goose.EnsureDBVersion(db.DB); err != nil {
		return fmt.Errorf("create goose version table: %w", err)
	}
	if _, err := db.Exec(
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (1, true)`); err != nil {
		return fmt.Errorf("stamp baseline version: %w", err)
	}
	return nil
}
