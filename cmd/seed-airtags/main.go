package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

type KeyEntry struct {
	Name                  string `json:"name"`
	UUID                  string `json:"uuid"`
	PrivateKey            string `json:"privateKey"`
	SharedSecret          string `json:"sharedSecret"`
	SecondarySharedSecret string `json:"secondarySharedSecret"`
	PairingDate           string `json:"pairingDate"`
	ProductID             int64  `json:"productId"`
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: seed-airtags <email> <password> <keys.json>")
		fmt.Println("  DATABASE_URL env var must be set")
		os.Exit(1)
	}

	email := os.Args[1]
	password := os.Args[2]
	keysFile := os.Args[3]

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL env var required")
	}

	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %v", err)
	}
	defer db.Close()

	// Read keys file
	data, err := os.ReadFile(keysFile)
	if err != nil {
		log.Fatalf("Failed to read keys file: %v", err)
	}

	var keys []KeyEntry
	if err := json.Unmarshal(data, &keys); err != nil {
		log.Fatalf("Failed to parse keys JSON: %v", err)
	}

	now := time.Now().Unix()

	// Upsert account
	accountID := uuid.New().String()
	err = db.QueryRow(
		`INSERT INTO airtag_accounts (id, email, password, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (email) DO UPDATE SET password = $3, updated_at = $5
		 RETURNING id`,
		accountID, email, password, now, now,
	).Scan(&accountID)
	if err != nil {
		log.Fatalf("Failed to upsert account: %v", err)
	}
	fmt.Printf("✅ Account: %s (id: %s)\n", email, accountID)

	// Delete existing keys for this account (fresh seed)
	res, _ := db.Exec(`DELETE FROM airtag_keys WHERE account_id = $1`, accountID)
	deleted, _ := res.RowsAffected()
	if deleted > 0 {
		fmt.Printf("   Cleared %d existing keys\n", deleted)
	}

	// Insert keys
	inserted := 0
	for _, k := range keys {
		_, err := db.Exec(
			`INSERT INTO airtag_keys (id, account_id, name, tag_uuid, private_key, shared_secret, secondary_shared_secret, pairing_date, product_id, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			uuid.New().String(), accountID, k.Name, k.UUID, k.PrivateKey, k.SharedSecret, k.SecondarySharedSecret, k.PairingDate, k.ProductID, now,
		)
		if err != nil {
			log.Printf("⚠️  Failed to insert key %s: %v", k.Name, err)
			continue
		}
		inserted++
	}

	fmt.Printf("✅ Inserted %d keys for %s\n", inserted, email)
}
