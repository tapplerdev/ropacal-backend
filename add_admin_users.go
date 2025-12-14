package main

import (
	"log"
	"os"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	// Get database connection string from environment
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

	log.Println("🔌 Connected to database")

	// Hash password for admin accounts (admin123)
	adminPassword, err := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("Failed to hash admin password: %v", err)
	}

	// Hash password for driver account (driver123)
	driverPassword, err := bcrypt.GenerateFromPassword([]byte("driver123"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("Failed to hash driver password: %v", err)
	}

	users := []map[string]interface{}{
		{
			"id":       uuid.New().String(),
			"email":    "nate@ropacal.com",
			"password": string(adminPassword),
			"name":     "Nate",
			"role":     "admin",
		},
		{
			"id":       uuid.New().String(),
			"email":    "ariel@ropacal.com",
			"password": string(adminPassword),
			"name":     "Ariel",
			"role":     "admin",
		},
		{
			"id":       uuid.New().String(),
			"email":    "daniel@ropacal.com",
			"password": string(driverPassword),
			"name":     "Daniel",
			"role":     "driver",
		},
	}

	for _, user := range users {
		// Check if user already exists
		var exists bool
		err := db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)", user["email"])
		if err != nil {
			log.Printf("❌ Error checking for user %s: %v", user["email"], err)
			continue
		}

		if exists {
			log.Printf("⚠️  User already exists: %s", user["email"])
			continue
		}

		// Insert new user
		query := `
			INSERT INTO users (id, email, password, name, role)
			VALUES (:id, :email, :password, :name, :role)
		`
		if _, err := db.NamedExec(query, user); err != nil {
			log.Printf("❌ Failed to create user %s: %v", user["email"], err)
			continue
		}

		log.Printf("✅ Created %s user: %s", user["role"], user["email"])
	}

	log.Println("\n📧 Login credentials:")
	log.Println("  nate@ropacal.com / admin123 (admin)")
	log.Println("  ariel@ropacal.com / admin123 (admin)")
	log.Println("  daniel@ropacal.com / driver123 (driver)")
}
