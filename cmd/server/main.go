package main

import (
	"log"
	"net/http"
	"os"

	"ropacal-backend/internal/database"
	"ropacal-backend/internal/handlers"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/websocket"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/joho/godotenv"
)

func main() {
	log.Println("═══════════════════════════════════════════════════════════════════")
	log.Println("🚀 ROPACAL BACKEND SERVER STARTING")
	log.Println("═══════════════════════════════════════════════════════════════════")

	// Load .env file
	log.Println("📂 Loading environment variables...")
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️  Warning: .env file not found, using environment variables from system")
	} else {
		log.Println("✅ .env file loaded successfully")
	}

	// Get database URL
	log.Println("🔍 Checking DATABASE_URL environment variable...")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ FATAL ERROR: DATABASE_URL environment variable is required")
		log.Println("   Please set DATABASE_URL in Railway Variables or .env file")
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Fatal("DATABASE_URL environment variable is required")
	}
	log.Println("✅ DATABASE_URL found")

	// Connect to database
	log.Println("🔌 Connecting to database...")
	db, err := database.Connect(dbURL)
	if err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ FATAL ERROR: Database connection failed")
		log.Printf("   Error: %v", err)
		log.Println("   This is usually caused by:")
		log.Println("   1. Wrong DATABASE_URL format")
		log.Println("   2. PostgreSQL service is down")
		log.Println("   3. Network connectivity issue")
		log.Println("   4. Invalid credentials")
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Fatal(err)
	}
	defer db.Close()
	log.Println("✅ Database connection established")

	// Run migrations
	log.Println("🔄 Running database migrations...")
	if err := database.Migrate(db); err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ FATAL ERROR: Database migrations failed")
		log.Printf("   Error: %v", err)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Fatal(err)
	}
	log.Println("✅ Database migrations completed")

	// Seed database
	log.Println("🌱 Seeding database with initial data...")
	if err := database.SeedUsers(db); err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ FATAL ERROR: User seeding failed")
		log.Printf("   Error: %v", err)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Fatal(err)
	}
	log.Println("✅ Users seeded successfully")

	if err := database.SeedBins(db); err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ FATAL ERROR: Bins seeding failed")
		log.Printf("   Error: %v", err)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Fatal(err)
	}
	log.Println("✅ Bins seeded successfully")

	// Initialize Firebase Cloud Messaging
	// Supports both file path and base64-encoded credentials (for Railway/cloud deployments)
	var fcmService *services.FCMService
	fcmCredsBase64 := os.Getenv("FIREBASE_CREDENTIALS_BASE64")

	if fcmCredsBase64 != "" {
		// Use base64-encoded credentials (Railway-friendly)
		fcmService, err = services.NewFCMServiceFromBase64(fcmCredsBase64)
		if err != nil {
			log.Printf("⚠️  Failed to initialize FCM from base64: %v (push notifications disabled)", err)
			fcmService = nil
		} else {
			log.Println("✅ Firebase Cloud Messaging initialized from base64 credentials")
		}
	} else {
		// Fall back to file path (local development)
		fcmCredentialsFile := os.Getenv("FIREBASE_CREDENTIALS_FILE")
		if fcmCredentialsFile == "" {
			fcmCredentialsFile = "./firebase-service-account.json"
		}

		fcmService, err = services.NewFCMService(fcmCredentialsFile)
		if err != nil {
			log.Printf("⚠️  Failed to initialize FCM from file: %v (push notifications disabled)", err)
			fcmService = nil
		} else {
			log.Println("✅ Firebase Cloud Messaging initialized from file")
		}
	}

	// Initialize WebSocket hub
	wsHub := websocket.NewHub()
	go wsHub.Run()
	log.Println("✅ WebSocket hub started")

	// Create router
	r := chi.NewRouter()

	// Middleware
	r.Use(chimiddleware.Logger)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)

	// CORS
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Authorization"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	// Health check
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	// Authentication routes (no auth required)
	r.Post("/api/auth/login", handlers.Login(db))

	// WebSocket endpoint (authentication handled in handler via query param)
	r.Get("/ws", websocket.HandleWebSocket(wsHub, db))

	// API routes
	r.Route("/api", func(r chi.Router) {
		// Bins endpoints
		r.Get("/bins", handlers.GetBins(db))
		r.Post("/bins", handlers.CreateBin(db))
		r.Patch("/bins/{id}", handlers.UpdateBin(db))
		r.Delete("/bins/{id}", handlers.DeleteBin(db))
		r.Get("/bins/top-performers", handlers.GetTopPerformingBins(db))

		// Checks endpoints
		r.Get("/bins/{id}/checks", handlers.GetChecks(db))
		r.Get("/checks", handlers.GetAllChecks(db))

		// Moves endpoints
		r.Get("/bins/{id}/moves", handlers.GetMoves(db))
		r.Post("/bins/{id}/moves", handlers.CreateMove(db))

		// Route optimization
		r.Post("/route", handlers.OptimizeRoute(db))

		// No-Go Zones endpoints
		r.Get("/no-go-zones", handlers.GetNoGoZones(db))
		r.Get("/no-go-zones/{id}", handlers.GetNoGoZone(db))
		r.Get("/no-go-zones/{id}/incidents", handlers.GetZoneIncidents(db))

		// Shift-related incident queries
		// TODO: Implement GetShiftIncidents handler
		// r.Get("/shifts/{id}/incidents", handlers.GetShiftIncidents(db))

		// Analytics endpoints
		r.Get("/analytics/areas", handlers.GetAreaPerformance(db))

		// Driver shift endpoints (require authentication)
		r.Group(func(r chi.Router) {
			r.Use(middleware.Auth)

			// Auth status endpoint
			r.Get("/auth/status", handlers.GetAuthStatus(db))

			// Shift management
			r.Get("/driver/shift/current", handlers.GetCurrentShift(db))
			r.Post("/driver/shift/start", handlers.StartShift(db, wsHub))
			r.Post("/driver/shift/pause", handlers.PauseShift(db, wsHub))
			r.Post("/driver/shift/resume", handlers.ResumeShift(db, wsHub))
			r.Post("/driver/shift/end", handlers.EndShift(db, wsHub))
			r.Post("/driver/shift/complete-bin", handlers.CompleteBin(db, wsHub))

			// Shift history
			r.Get("/driver/shift-history", handlers.GetDriverShiftHistory(db))
			r.Get("/driver/shift-details", handlers.GetShiftDetails(db))

			// Location tracking (sent every 10 seconds during active shift)
			r.Post("/driver/location", handlers.UpdateLocation(db, wsHub))

			// FCM token registration
			r.Post("/driver/fcm-token", handlers.RegisterFCMToken(db))

			// Incident reporting (drivers can report both check-based and field observations)
			// TODO: Implement CreateZoneIncident handler (currently handled in CompleteBin)
			// r.Post("/zone-incidents", handlers.CreateZoneIncident(db))
		})

		// Diagnostic logging endpoint (no auth required for easier debugging)
		r.Post("/api/logs/diagnostic", handlers.ReceiveDiagnosticLog(db))

		// Manager endpoints (require authentication + admin role)
		r.Group(func(r chi.Router) {
			r.Use(middleware.Auth)
			r.Use(middleware.RequireRole("admin"))

			r.Post("/manager/assign-route", handlers.AssignRoute(db, wsHub, fcmService))
			r.Delete("/manager/shifts/clear", handlers.ClearAllShifts(db, wsHub))

			// One-time data migration endpoints (can be removed after use)
			r.Post("/manager/bins/load-real", handlers.LoadRealBins(db))
			r.Post("/manager/bins/fix-status", handlers.FixBinStatus(db))

			// Fleet management
			r.Get("/manager/drivers", handlers.GetAllDrivers(db))
			r.Get("/manager/active-drivers", handlers.GetActiveDrivers(db))
			r.Get("/manager/driver-shift-details", handlers.GetDriverShiftDetails(db))

			// User management
			r.Post("/users", handlers.CreateUser(db))

			// No-Go Zone management (admin only)
			// TODO: Implement admin zone management handlers
			// r.Post("/no-go-zones", handlers.CreateNoGoZone(db))
			// r.Patch("/no-go-zones/{id}", handlers.UpdateNoGoZone(db))
			// r.Delete("/no-go-zones/{id}", handlers.DeleteNoGoZone(db))

			// Field observations management
			// TODO: Implement field observation management handlers
			// r.Get("/field-observations", handlers.GetFieldObservations(db))
			// r.Patch("/field-observations/{id}/verify", handlers.VerifyFieldObservation(db))
		})
	})

	// Get port
	log.Println("🔍 Checking PORT environment variable...")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
		log.Printf("⚠️  PORT not set, using default: %s", port)
	} else {
		log.Printf("✅ PORT found: %s", port)
	}

	log.Println("═══════════════════════════════════════════════════════════════════")
	log.Println("✅ ALL INITIALIZATION COMPLETE")
	log.Printf("🚀 Server starting on http://localhost:%s", port)
	log.Println("🔌 Ready to accept requests!")
	log.Println("═══════════════════════════════════════════════════════════════════")

	// Start server
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ FATAL ERROR: Server failed to start")
		log.Printf("   Error: %v", err)
		log.Printf("   Port: %s", port)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Fatal(err)
	}
}
