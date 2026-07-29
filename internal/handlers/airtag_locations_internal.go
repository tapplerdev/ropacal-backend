package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/orgdb"

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

// TENANCY: no user JWT here (INTERNAL_API_KEY only) — each row's owning org
// is RESOLVED by probing the active orgs' scopes (existing airtag row by id,
// airtag_keys by tag name, bins by bin_number) with an in-process 1h cache;
// see airtag_org.go for the contract. Dark mode short-circuits to the single
// passthrough handle: no probes, byte-identical behavior. Unresolvable rows
// (owned by no org, or ambiguously by several) are skipped loudly, mirroring
// this endpoint's established per-row error handling; a batch where NOTHING
// resolves returns 404.

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
		resolved := 0

		// Orgs that received rows this sync, for the per-org last-sync stamp.
		// Dark mode pre-seeds the single passthrough so the stamp is written
		// unconditionally, exactly as today (SyncedAt set + zero rows still
		// stamps).
		touched := map[string]*orgdb.DB{}
		if !orgdb.Migrated() {
			touched[""] = orgdb.Passthrough(db)
		}

		// Upsert matched bin locations
		for _, loc := range payload.Locations {
			id := loc.ID
			if id == "" && loc.BinNumber != nil {
				id = fmt.Sprintf("%d", *loc.BinNumber)
			}
			odb, found, err := resolveAirtagOrg(db, "airtag_loc:"+id, airtagLocationProbe(id, loc.Name, loc.BinNumber))
			if err != nil {
				log.Printf("❌ [AirtagLocations] Org resolution failed for %s: %v", id, err)
				continue
			}
			if !found {
				log.Printf("🚫 [AirtagLocations] No single organization owns %s (name=%q) — row skipped", id, loc.Name)
				continue
			}
			resolved++
			_, err = odb.Exec(`
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
			touched[odb.OrgID()] = odb
		}

		// Upsert unmatched tag locations
		for _, loc := range payload.Unmatched {
			id := "unmatched:" + sanitizeTagName(loc.Name)
			odb, found, err := resolveAirtagOrg(db, "airtag_loc:"+id, airtagLocationProbe(id, loc.Name, nil))
			if err != nil {
				log.Printf("❌ [AirtagLocations] Org resolution failed for unmatched %s: %v", id, err)
				continue
			}
			if !found {
				log.Printf("🚫 [AirtagLocations] No single organization owns unmatched %s (name=%q) — row skipped", id, loc.Name)
				continue
			}
			resolved++
			_, err = odb.Exec(`
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
			touched[odb.OrgID()] = odb
		}

		// Absent everywhere: tenancy is live, the batch had entries, and not
		// one belongs to any active org. Tell the bridge so the failure is a
		// visible 404, not a quietly-empty 200.
		if orgdb.Migrated() && len(payload.Locations)+len(payload.Unmatched) > 0 && resolved == 0 {
			http.Error(w, "No organization owns any of the submitted airtags", http.StatusNotFound)
			return
		}

		// Store last sync timestamp in config, per touched tenant (the single
		// passthrough while dark — one write, exactly as before). The conflict
		// target is (key) pre-migration and (organization_id, key) once
		// tenancy is live; organization_id itself is filled by the migration's
		// GUC DEFAULT through the org-bound handle.
		if payload.SyncedAt != "" {
			syncValue := fmt.Sprintf(`{"last_sync_at": "%s"}`, payload.SyncedAt)
			for _, h := range touched {
				_, err := h.Exec(fmt.Sprintf(`
					INSERT INTO config (key, value, updated_by)
					VALUES ('airtag_last_sync', $1::jsonb, 'bridge')
					ON CONFLICT %s DO UPDATE SET value = $1::jsonb, updated_at = CURRENT_TIMESTAMP
				`, orgdb.ConfigConflictTarget()), syncValue)
				if err != nil {
					log.Printf("❌ [AirtagLocations] Failed to update last_sync config: %v", err)
				}
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
