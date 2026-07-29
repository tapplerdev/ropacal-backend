package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"ropacal-backend/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// TENANCY NOTE: this file deliberately keeps the raw *sqlx.DB pool (no
// orgdb shadow). Its endpoints carry no user JWT (server-to-server /
// INTERNAL_API_KEY paths), so there is no organization to bind — under RLS
// with a non-superuser role their queries fail closed (zero rows / NOT NULL
// on insert) rather than crossing tenants. Scoping these paths needs a
// caller-identity -> org mapping first (see the tenancy workers audit,
// sections 4E/4F) and is tracked as follow-up work; do NOT "fix" them by
// adding an unscoped bypass inside orgdb.

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

// GetAirtagAccounts returns all airtag accounts with their keys
func GetAirtagAccounts(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var accounts []models.AirtagAccount
		err := db.Select(&accounts, `SELECT id, email, password, account_state, created_at, updated_at FROM airtag_accounts ORDER BY created_at`)
		if err != nil {
			log.Printf("❌ Failed to fetch airtag accounts: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		var keys []models.AirtagKey
		err = db.Select(&keys, `SELECT id, account_id, name, tag_uuid, private_key, shared_secret, secondary_shared_secret, pairing_date, product_id, created_at FROM airtag_keys ORDER BY name`)
		if err != nil {
			log.Printf("❌ Failed to fetch airtag keys: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
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

		now := time.Now().Unix()
		result, err := db.Exec(
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
