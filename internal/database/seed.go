package database

import (
	"log"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

func SeedBins(db *sqlx.DB) error {
	// Check if bins already exist
	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM bins"); err != nil {
		return err
	}

	if count > 0 {
		log.Println("✓ Bins already seeded, skipping...")
		return nil
	}

	log.Println("🌱 Seeding 44 bins...")

	bins := []map[string]interface{}{
		{"bin_number": 1, "current_street": "325 S 1st St", "city": "San Jose", "zip": "95113", "fill_percentage": 45, "status": "Active", "latitude": 37.3329, "longitude": -121.8866},
		{"bin_number": 2, "current_street": "200 E Santa Clara St", "city": "San Jose", "zip": "95113", "fill_percentage": 67, "status": "Active", "latitude": 37.3361, "longitude": -121.8869},
		{"bin_number": 3, "current_street": "151 W Mission St", "city": "San Jose", "zip": "95110", "fill_percentage": 23, "status": "Active", "latitude": 37.3343, "longitude": -121.8936},
		{"bin_number": 4, "current_street": "408 Almaden Blvd", "city": "San Jose", "zip": "95110", "fill_percentage": 89, "status": "Active", "latitude": 37.3313, "longitude": -121.8917},
		{"bin_number": 5, "current_street": "180 Park Ave", "city": "San Jose", "zip": "95113", "fill_percentage": 12, "status": "Active", "latitude": 37.3351, "longitude": -121.8894},
		{"bin_number": 6, "current_street": "72 N Almaden Ave", "city": "San Jose", "zip": "95110", "fill_percentage": 78, "status": "Active", "latitude": 37.3352, "longitude": -121.8931},
		{"bin_number": 7, "current_street": "345 E Santa Clara St", "city": "San Jose", "zip": "95113", "fill_percentage": 56, "status": "Active", "latitude": 37.3357, "longitude": -121.8826},
		{"bin_number": 8, "current_street": "99 S Market St", "city": "San Jose", "zip": "95113", "fill_percentage": 34, "status": "Active", "latitude": 37.3339, "longitude": -121.8905},
		{"bin_number": 9, "current_street": "201 S 2nd St", "city": "San Jose", "zip": "95113", "fill_percentage": 91, "status": "Active", "latitude": 37.3326, "longitude": -121.8863},
		{"bin_number": 10, "current_street": "150 S 1st St", "city": "San Jose", "zip": "95113", "fill_percentage": 15, "status": "Active", "latitude": 37.3344, "longitude": -121.8877},
		{"bin_number": 11, "current_street": "88 W San Carlos St", "city": "San Jose", "zip": "95113", "fill_percentage": 82, "status": "Active", "latitude": 37.3307, "longitude": -121.8901},
		{"bin_number": 12, "current_street": "250 S 3rd St", "city": "San Jose", "zip": "95112", "fill_percentage": 47, "status": "Active", "latitude": 37.3311, "longitude": -121.8842},
		{"bin_number": 13, "current_street": "123 N 4th St", "city": "San Jose", "zip": "95112", "fill_percentage": 63, "status": "Active", "latitude": 37.3389, "longitude": -121.8822},
		{"bin_number": 14, "current_street": "456 W San Fernando St", "city": "San Jose", "zip": "95113", "fill_percentage": 29, "status": "Active", "latitude": 37.3323, "longitude": -121.8955},
		{"bin_number": 15, "current_street": "789 E Julian St", "city": "San Jose", "zip": "95112", "fill_percentage": 71, "status": "Active", "latitude": 37.3442, "longitude": -121.8793},
		{"bin_number": 16, "current_street": "321 N 1st St", "city": "San Jose", "zip": "95112", "fill_percentage": 38, "status": "Active", "latitude": 37.3423, "longitude": -121.8878},
		{"bin_number": 17, "current_street": "654 E St John St", "city": "San Jose", "zip": "95112", "fill_percentage": 95, "status": "Active", "latitude": 37.3473, "longitude": -121.8786},
		{"bin_number": 18, "current_street": "147 S 4th St", "city": "San Jose", "zip": "95112", "fill_percentage": 19, "status": "Active", "latitude": 37.3341, "longitude": -121.8828},
		{"bin_number": 19, "current_street": "258 W St James St", "city": "San Jose", "zip": "95110", "fill_percentage": 86, "status": "Active", "latitude": 37.3385, "longitude": -121.8972},
		{"bin_number": 20, "current_street": "369 E San Salvador St", "city": "San Jose", "zip": "95112", "fill_percentage": 52, "status": "Active", "latitude": 37.3289, "longitude": -121.8816},
		{"bin_number": 21, "current_street": "741 S 5th St", "city": "San Jose", "zip": "95112", "fill_percentage": 44, "status": "Active", "latitude": 37.3267, "longitude": -121.8807},
		{"bin_number": 22, "current_street": "852 N 6th St", "city": "San Jose", "zip": "95112", "fill_percentage": 76, "status": "Active", "latitude": 37.3512, "longitude": -121.8789},
		{"bin_number": 23, "current_street": "963 E Empire St", "city": "San Jose", "zip": "95112", "fill_percentage": 31, "status": "Active", "latitude": 37.3531, "longitude": -121.8771},
		{"bin_number": 24, "current_street": "159 S 7th St", "city": "San Jose", "zip": "95112", "fill_percentage": 68, "status": "Active", "latitude": 37.3336, "longitude": -121.8774},
		{"bin_number": 25, "current_street": "267 W San Carlos St", "city": "San Jose", "zip": "95110", "fill_percentage": 25, "status": "Active", "latitude": 37.3297, "longitude": -121.8954},
		{"bin_number": 26, "current_street": "378 E Reed St", "city": "San Jose", "zip": "95112", "fill_percentage": 93, "status": "Active", "latitude": 37.3248, "longitude": -121.8802},
		{"bin_number": 27, "current_street": "489 N 8th St", "city": "San Jose", "zip": "95112", "fill_percentage": 17, "status": "Active", "latitude": 37.3467, "longitude": -121.8752},
		{"bin_number": 28, "current_street": "591 S 9th St", "city": "San Jose", "zip": "95112", "fill_percentage": 84, "status": "Active", "latitude": 37.3289, "longitude": -121.8734},
		{"bin_number": 29, "current_street": "692 E William St", "city": "San Jose", "zip": "95112", "fill_percentage": 41, "status": "Active", "latitude": 37.3421, "longitude": -121.8767},
		{"bin_number": 30, "current_street": "793 W Julian St", "city": "San Jose", "zip": "95126", "fill_percentage": 79, "status": "Active", "latitude": 37.3439, "longitude": -121.9033},
		{"bin_number": 31, "current_street": "894 N 10th St", "city": "San Jose", "zip": "95112", "fill_percentage": 36, "status": "Active", "latitude": 37.3523, "longitude": -121.8714},
		{"bin_number": 32, "current_street": "195 S 11th St", "city": "San Jose", "zip": "95112", "fill_percentage": 88, "status": "Active", "latitude": 37.3321, "longitude": -121.8693},
		{"bin_number": 33, "current_street": "296 E Santa Clara St", "city": "San Jose", "zip": "95113", "fill_percentage": 22, "status": "Active", "latitude": 37.3359, "longitude": -121.8841},
		{"bin_number": 34, "current_street": "397 N 12th St", "city": "San Jose", "zip": "95112", "fill_percentage": 74, "status": "Active", "latitude": 37.3489, "longitude": -121.8671},
		{"bin_number": 35, "current_street": "498 S Market St", "city": "San Jose", "zip": "95113", "fill_percentage": 59, "status": "Active", "latitude": 37.3278, "longitude": -121.8877},
		{"bin_number": 36, "current_street": "599 E San Fernando St", "city": "San Jose", "zip": "95112", "fill_percentage": 33, "status": "Active", "latitude": 37.3318, "longitude": -121.8781},
		{"bin_number": 37, "current_street": "691 N 13th St", "city": "San Jose", "zip": "95112", "fill_percentage": 97, "status": "Active", "latitude": 37.3501, "longitude": -121.8652},
		{"bin_number": 38, "current_street": "792 W Santa Clara St", "city": "San Jose", "zip": "95126", "fill_percentage": 28, "status": "Active", "latitude": 37.3351, "longitude": -121.9098},
		{"bin_number": 39, "current_street": "893 S 14th St", "city": "San Jose", "zip": "95112", "fill_percentage": 81, "status": "Active", "latitude": 37.3256, "longitude": -121.8631},
		{"bin_number": 40, "current_street": "994 E St John St", "city": "San Jose", "zip": "95112", "fill_percentage": 46, "status": "Active", "latitude": 37.3471, "longitude": -121.8622},
		{"bin_number": 41, "current_street": "195 N 15th St", "city": "San Jose", "zip": "95112", "fill_percentage": 72, "status": "Active", "latitude": 37.3434, "longitude": -121.8601},
		{"bin_number": 42, "current_street": "296 S 16th St", "city": "San Jose", "zip": "95112", "fill_percentage": 39, "status": "Active", "latitude": 37.3298, "longitude": -121.8579},
		{"bin_number": 43, "current_street": "397 E Julian St", "city": "San Jose", "zip": "95112", "fill_percentage": 85, "status": "Active", "latitude": 37.3443, "longitude": -121.8823},
		{"bin_number": 44, "current_street": "498 N 17th St", "city": "San Jose", "zip": "95112", "fill_percentage": 54, "status": "Active", "latitude": 37.3456, "longitude": -121.8558},
	}

	for _, bin := range bins {
		id := uuid.New().String()
		_, err := db.Exec(`
			INSERT INTO bins (id, bin_number, current_street, city, zip, status, fill_percentage, latitude, longitude, checked, move_requested)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0, 0)
		`, id, bin["bin_number"], bin["current_street"], bin["city"], bin["zip"], bin["status"], bin["fill_percentage"], bin["latitude"], bin["longitude"])

		if err != nil {
			return err
		}
	}

	log.Println("✓ Successfully seeded 44 bins")
	return nil
}

func SeedUsers(db *sqlx.DB) error {
	// Check if users already exist
	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM users"); err != nil {
		return err
	}

	if count > 0 {
		log.Println("✓ Users already seeded, skipping...")
		return nil
	}

	log.Println("🌱 Seeding test users...")

	// Hash passwords
	driverPassword, err := bcrypt.GenerateFromPassword([]byte("driver123"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	adminPassword, err := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	users := []map[string]interface{}{
		{
			"id":       uuid.New().String(),
			"email":    "driver@ropacal.com",
			"password": string(driverPassword),
			"name":     "John Driver",
			"role":     "driver",
		},
		{
			"id":       uuid.New().String(),
			"email":    "admin@ropacal.com",
			"password": string(adminPassword),
			"name":     "Admin User",
			"role":     "admin",
		},
	}

	for _, user := range users {
		query := `
			INSERT INTO users (id, email, password, name, role)
			VALUES (:id, :email, :password, :name, :role)
		`
		if _, err := db.NamedExec(query, user); err != nil {
			return err
		}
		log.Printf("  ✓ Created user: %s (%s)", user["email"], user["role"])
	}

	log.Println("✓ Successfully seeded test users")
	log.Println("  📧 Driver: driver@ropacal.com / driver123")
	log.Println("  📧 Admin:  admin@ropacal.com / admin123")
	return nil
}
