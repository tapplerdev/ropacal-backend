package main

import (
	"fmt"
	"log"
	"os"

	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	// Get database URL
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable not set")
	}

	// Connect to database
	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	log.Println("Connected to database successfully")

	// Read and execute migration file
	migrationPath := "migrations/load_real_bins.sql"
	migrationSQL, err := os.ReadFile(migrationPath)
	if err != nil {
		log.Fatalf("Failed to read migration file: %v", err)
	}

	log.Printf("Executing migration: %s\n", migrationPath)

	// Execute the migration
	_, err = db.Exec(string(migrationSQL))
	if err != nil {
		log.Fatalf("Migration failed: %v", err)
	}

	log.Println("Migration completed successfully!")

	// Query and display summary
	var result struct {
		Message             string `db:"message"`
		TotalBinsLoaded     int    `db:"total_bins_loaded"`
		BinsWithoutCoords   int    `db:"bins_without_coords"`
		ActiveBins          int    `db:"active_bins"`
		MissingBins         int    `db:"missing_bins"`
	}

	query := `
		SELECT
			'Migration completed successfully' AS message,
			COUNT(*) AS total_bins_loaded,
			COUNT(CASE WHEN latitude IS NULL OR longitude IS NULL THEN 1 END) AS bins_without_coords,
			COUNT(CASE WHEN status = 'Active' THEN 1 END) AS active_bins,
			COUNT(CASE WHEN status = 'Missing' THEN 1 END) AS missing_bins
		FROM bins
		WHERE city != 'Dallas'
	`

	err = db.Get(&result, query)
	if err != nil {
		log.Fatalf("Failed to query summary: %v", err)
	}

	// Display results
	fmt.Println("\n============================================================")
	fmt.Println("MIGRATION SUMMARY")
	fmt.Println("============================================================")
	fmt.Printf("Total bins loaded:       %d\n", result.TotalBinsLoaded)
	fmt.Printf("Active bins:             %d\n", result.ActiveBins)
	fmt.Printf("Missing bins:            %d\n", result.MissingBins)
	fmt.Printf("Bins without coords:     %d (won't show on map)\n", result.BinsWithoutCoords)
	fmt.Println("============================================================")
}
