package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"ropacal-backend/internal/models"
	"ropacal-backend/internal/orgdb"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// TENANCY: these endpoints carry no user JWT (INTERNAL_API_KEY only), so the
// owning organization is RESOLVED by probing each active org's scope — see
// airtag_org.go for the full contract (dark mode = passthrough, byte-identical;
// zero/ambiguous owners fail closed).

// InternalAPIKey middleware validates the INTERNAL_API_KEY header
func InternalAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := os.Getenv("INTERNAL_API_KEY")
		if apiKey == "" {
			log.Println("❌ INTERNAL_API_KEY not configured")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		provided := r.Header.Get("X-Internal-API-Key")
		if provided != apiKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// GetAirtagAccounts returns all airtag accounts with their keys.
// The bridge polls every tenant's tags in one sweep, so once tenancy is live
// this fans out over the active orgs and unions the results (a single
// passthrough iteration while dark — byte-identical). Any per-org failure
// fails the whole request: a silently partial account list would stop the
// bridge polling that tenant's tags.
func GetAirtagAccounts(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handles, err := airtagOrgHandles(db)
		if err != nil {
			log.Printf("❌ Failed to enumerate organizations for airtag accounts: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		var accounts []models.AirtagAccount
		var keys []models.AirtagKey
		for _, h := range handles {
			var orgAccounts []models.AirtagAccount
			err := h.Select(&orgAccounts, `SELECT id, email, password, account_state, created_at, updated_at FROM airtag_accounts ORDER BY created_at`)
			if err != nil {
				log.Printf("❌ Failed to fetch airtag accounts: %v", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			accounts = append(accounts, orgAccounts...)

			var orgKeys []models.AirtagKey
			err = h.Select(&orgKeys, `SELECT id, account_id, name, tag_uuid, private_key, shared_secret, secondary_shared_secret, pairing_date, product_id, created_at FROM airtag_keys ORDER BY name`)
			if err != nil {
				log.Printf("❌ Failed to fetch airtag keys: %v", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			keys = append(keys, orgKeys...)
		}

		// Group keys by account_id
		keysByAccount := make(map[string][]models.AirtagKey)
		for _, key := range keys {
			keysByAccount[key.AccountID] = append(keysByAccount[key.AccountID], key)
		}

		// Build response
		result := make([]models.AirtagAccountWithKeys, len(accounts))
		for i, acc := range accounts {
			accKeys := keysByAccount[acc.ID]
			if accKeys == nil {
				accKeys = []models.AirtagKey{}
			}
			result[i] = models.AirtagAccountWithKeys{
				ID:           acc.ID,
				Email:        acc.Email,
				Password:     acc.Password,
				AccountState: acc.AccountState,
				Keys:         accKeys,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

// UpdateAirtagAccountState updates the session state for an account
func UpdateAirtagAccountState(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := chi.URLParam(r, "id")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		var payload struct {
			AccountState string `json:"account_state"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		// Resolve the org owning this account (passthrough while dark).
		// Absent in every active org → the same 404 the zero-rows UPDATE
		// produces today.
		odb, found, err := resolveAirtagOrg(db, "airtag_account:"+accountID, func(d *orgdb.DB) (bool, error) {
			var exists bool
			err := d.Get(&exists, `SELECT EXISTS(SELECT 1 FROM airtag_accounts WHERE id = $1)`, accountID)
			return exists, err
		})
		if err != nil {
			log.Printf("❌ Failed to resolve organization for airtag account %s: %v", accountID, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "Account not found", http.StatusNotFound)
			return
		}

		now := time.Now().Unix()
		result, err := odb.Exec(
			`UPDATE airtag_accounts SET account_state = $1, updated_at = $2 WHERE id = $3`,
			payload.AccountState, now, accountID,
		)
		if err != nil {
			log.Printf("❌ Failed to update airtag account state: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		rows, _ := result.RowsAffected()
		if rows == 0 {
			http.Error(w, "Account not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
