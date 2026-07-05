package itinerary

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ── Creation-enrichment resolvers (Phase 5 structural) ───────────────────────
//
// Typed snapshots backing CreateShiftWithTasks' client-first enrichment: the
// caller merges these into the payload (fill-missing-keys), the domain owns
// the reads. Each is an EXACT mirror of the historical inline lookup —
// including scan strictness: non-pointer fields mean a row with NULLs errors
// the whole lookup, which the create path treats as non-fatal "no enrichment"
// (a NULL-coordinate bin gets NO enrichment at all, not even bin_number).
// Loosening these to pointers, or sharing them with the add_tasks path (whose
// scans 400 where this path silently skips), is a FLAGGED behavior change —
// not a refactor.

// ResolveBinStatus returns a bin's status — the create-path safety net that
// skips retired/missing/in_storage bins.
func ResolveBinStatus(ext sqlx.Ext, binID string) (string, error) {
	var status string
	err := sqlx.Get(ext, &status, ext.Rebind(`SELECT status FROM bins WHERE id = ?`), binID)
	return status, err
}

// BinCoords is a bin's bare location — the pickup-at-0,0 coordinate fallback.
type BinCoords struct {
	Latitude  float64 `db:"latitude"`
	Longitude float64 `db:"longitude"`
}

// ResolveBinCoords returns a bin's coordinates (errors on NULL coords — the
// caller only applies non-zero results).
func ResolveBinCoords(ext sqlx.Ext, binID string) (BinCoords, error) {
	var c BinCoords
	err := sqlx.Get(ext, &c, ext.Rebind(`SELECT latitude, longitude FROM bins WHERE id = ?`), binID)
	return c, err
}

// BinEnrichment is a bin's creation snapshot: identity, fill and location.
type BinEnrichment struct {
	BinNumber      int     `db:"bin_number"`
	FillPercentage int     `db:"fill_percentage"`
	Latitude       float64 `db:"latitude"`
	Longitude      float64 `db:"longitude"`
	CurrentStreet  string  `db:"current_street"`
	City           string  `db:"city"`
	Zip            string  `db:"zip"`
}

// ComposedAddress is the create-path address form: "street, city zip".
func (b BinEnrichment) ComposedAddress() string {
	return fmt.Sprintf("%s, %s %s", b.CurrentStreet, b.City, b.Zip)
}

// ResolveBinEnrichment returns the bin fields the create path backfills.
func ResolveBinEnrichment(ext sqlx.Ext, binID string) (BinEnrichment, error) {
	var b BinEnrichment
	err := sqlx.Get(ext, &b, ext.Rebind(
		`SELECT bin_number, fill_percentage, latitude, longitude, current_street, city, zip FROM bins WHERE id = ?`), binID)
	return b, err
}

// PotentialLocationEnrichment is a potential location's creation snapshot.
type PotentialLocationEnrichment struct {
	Latitude  float64 `db:"latitude"`
	Longitude float64 `db:"longitude"`
	Street    string  `db:"street"`
	City      string  `db:"city"`
	Zip       string  `db:"zip"`
}

// ComposedAddress is the create-path potloc address form: "street, city, zip"
// (comma before the zip — historically different from the bin form).
func (p PotentialLocationEnrichment) ComposedAddress() string {
	return fmt.Sprintf("%s, %s, %s", p.Street, p.City, p.Zip)
}

// ResolvePotentialLocationEnrichment returns the potloc fields the create
// path backfills.
func ResolvePotentialLocationEnrichment(ext sqlx.Ext, id string) (PotentialLocationEnrichment, error) {
	var p PotentialLocationEnrichment
	err := sqlx.Get(ext, &p, ext.Rebind(
		`SELECT latitude, longitude, street, city, zip FROM potential_locations WHERE id = ?`), id)
	return p, err
}

// MoveEnrichment is a move request's creation snapshot: both legs' locations,
// the bin, and the move type (nullable columns stay pointers — matching the
// historical scan).
type MoveEnrichment struct {
	OriginalLatitude  *float64 `db:"original_latitude"`
	OriginalLongitude *float64 `db:"original_longitude"`
	OriginalAddress   *string  `db:"original_address"`
	NewLatitude       *float64 `db:"new_latitude"`
	NewLongitude      *float64 `db:"new_longitude"`
	NewAddress        *string  `db:"new_address"`
	BinID             string   `db:"bin_id"`
	MoveType          string   `db:"move_type"`
}

// ResolveMoveEnrichment returns the move fields the create path backfills.
func ResolveMoveEnrichment(ext sqlx.Ext, moveRequestID string) (MoveEnrichment, error) {
	var m MoveEnrichment
	err := sqlx.Get(ext, &m, ext.Rebind(
		`SELECT original_latitude, original_longitude, original_address, new_latitude, new_longitude, new_address, bin_id, move_type FROM bin_move_requests WHERE id = ?`), moveRequestID)
	return m, err
}

// WarehouseConfig is the live config warehouse — the authoritative store-move
// destination (never a shift snapshot or a possibly-stale move.new_*).
type WarehouseConfig struct {
	Lat  float64 `db:"lat"`
	Lng  float64 `db:"lng"`
	Addr string  `db:"addr"`
}

// ResolveWarehouseConfig reads the warehouse_location config key. Lat==0 in
// the result means unset/unusable (the caller's guard).
func ResolveWarehouseConfig(ext sqlx.Ext) (WarehouseConfig, error) {
	var w WarehouseConfig
	err := sqlx.Get(ext, &w, `SELECT
		COALESCE((SELECT (value::jsonb->>'latitude')::float8 FROM config WHERE key = 'warehouse_location'), 0) as lat,
		COALESCE((SELECT (value::jsonb->>'longitude')::float8 FROM config WHERE key = 'warehouse_location'), 0) as lng,
		COALESCE((SELECT value::jsonb->>'address' FROM config WHERE key = 'warehouse_location'), '') as addr`)
	return w, err
}

// ResolveBinNumber returns just the bin's number — the move-branch bin
// denormalization lookup.
func ResolveBinNumber(ext sqlx.Ext, binID string) (int, error) {
	var n int
	err := sqlx.Get(ext, &n, ext.Rebind(`SELECT bin_number FROM bins WHERE id = ?`), binID)
	return n, err
}
