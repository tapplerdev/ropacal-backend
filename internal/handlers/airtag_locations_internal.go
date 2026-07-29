package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

type upsertLocationsPayload struct {
	Locations []locationEntry `json:"locations"`
	Unmatched []locationEntry `json:"unmatched"`
	SyncedAt  string          `json:"synced_at"`
}

type locationEntry struct {
	ID            string  `json:"id"`
	BinNumber     *int    `json:"bin_number"`
	Name          string  `json:"name"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	Address       string  `json:"address"`
	City          string  `json:"city"`
	LastSeen      string  `json:"last_seen"`
	BatteryStatus int     `json:"battery_status"`
}

// TENANCY NOTE: this file deliberately keeps the raw *sqlx.DB pool (no
// orgdb shadow). Its endpoints carry no user JWT (server-to-server /
// INTERNAL_API_KEY paths), so there is no organization to bind — under RLS
// with a non-superuser role their queries fail closed (zero rows / NOT NULL
// on insert) rather than crossing tenants. Scoping these paths needs a
// caller-identity -> org mapping first (see the tenancy workers audit,
// sections 4E/4F) and is tracked as follow-up work; do NOT "fix" them by
// adding an unscoped bypass inside orgdb.

// UpsertAirtagLocations receives AirTag locations from the FindMy bridge and stores them in the DB.
func UpsertAirtagLocations(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		var payload upsertLocationsPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()
		upserted := 0

		// Upsert matched bin locations
		for _, loc := range payload.Locations {
			id := loc.ID
			if id == "" && loc.BinNumber != nil {
				id = fmt.Sprintf("%d", *loc.BinNumber)
			}
			_, err := db.Exec(`
				INSERT INTO airtag_locations (id, bin_number, name, latitude, longitude, address, city, last_seen, battery_status, is_matched, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8::timestamptz, $9, TRUE, $10)
				ON CONFLICT (id) DO UPDATE SET
					bin_number = EXCLUDED.bin_number,
					name = EXCLUDED.name,
					latitude = EXCLUDED.latitude,
					longitude = EXCLUDED.longitude,
					address = EXCLUDED.address,
					city = EXCLUDED.city,
					last_seen = EXCLUDED.last_seen,
					battery_status = EXCLUDED.battery_status,
					is_matched = TRUE,
					updated_at = EXCLUDED.updated_at
			`, id, loc.BinNumber, loc.Name, loc.Latitude, loc.Longitude,
				loc.Address, loc.City, loc.LastSeen, loc.BatteryStatus, now)
			if err != nil {
				log.Printf("❌ [AirtagLocations] Failed to upsert %s: %v", id, err)
				continue
			}
			upserted++
		}

		// Upsert unmatched tag locations
		for _, loc := range payload.Unmatched {
			id := "unmatched:" + sanitizeTagName(loc.Name)
			_, err := db.Exec(`
				INSERT INTO airtag_locations (id, bin_number, name, latitude, longitude, address, city, last_seen, battery_status, is_matched, updated_at)
				VALUES ($1, NULL, $2, $3, $4, $5, $6, $7::timestamptz, $8, FALSE, $9)
				ON CONFLICT (id) DO UPDATE SET
					name = EXCLUDED.name,
					latitude = EXCLUDED.latitude,
					longitude = EXCLUDED.longitude,
					address = EXCLUDED.address,
					city = EXCLUDED.city,
					last_seen = EXCLUDED.last_seen,
					battery_status = EXCLUDED.battery_status,
					is_matched = FALSE,
					updated_at = EXCLUDED.updated_at
			`, id, loc.Name, loc.Latitude, loc.Longitude,
				loc.Address, loc.City, loc.LastSeen, loc.BatteryStatus, now)
			if err != nil {
				log.Printf("❌ [AirtagLocations] Failed to upsert unmatched %s: %v", id, err)
				continue
			}
			upserted++
		}

		// Store last sync timestamp in config table
		if payload.SyncedAt != "" {
			syncValue := fmt.Sprintf(`{"last_sync_at": "%s"}`, payload.SyncedAt)
			_, err := db.Exec(`
				INSERT INTO config (key, value, updated_by)
				VALUES ('airtag_last_sync', $1::jsonb, 'bridge')
				ON CONFLICT (key) DO UPDATE SET value = $1::jsonb, updated_at = CURRENT_TIMESTAMP
			`, syncValue)
			if err != nil {
				log.Printf("❌ [AirtagLocations] Failed to update last_sync config: %v", err)
			}
		}

		log.Printf("✅ [AirtagLocations] Upserted %d/%d locations from bridge",
			upserted, len(payload.Locations)+len(payload.Unmatched))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "ok",
			"upserted": upserted,
		})
	}
}

func sanitizeTagName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "_")
}
