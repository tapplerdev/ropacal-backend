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
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found, using environment variables")
	}

	// Get database URL
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	// Connect to database
	db, err := database.Connect(dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Run migrations
	if err := database.Migrate(db); err != nil {
		log.Fatal(err)
	}

	// Seed database
	if err := database.SeedUsers(db); err != nil {
		log.Fatal(err)
	}
	if err := database.SeedBins(db); err != nil {
		log.Fatal(err)
	}

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
	r.Get("/ws", websocket.HandleWebSocket(wsHub))

	// API routes
	r.Route("/api", func(r chi.Router) {
		// Bins endpoints
		r.Get("/bins", handlers.GetBins(db))
		r.Post("/bins", handlers.CreateBin(db))
		r.Patch("/bins/{id}", handlers.UpdateBin(db))
		r.Delete("/bins/{id}", handlers.DeleteBin(db))

		// Checks endpoints
		r.Get("/bins/{id}/checks", handlers.GetChecks(db))

		// Moves endpoints
		r.Get("/bins/{id}/moves", handlers.GetMoves(db))
		r.Post("/bins/{id}/moves", handlers.CreateMove(db))

		// Route optimization
		r.Post("/route", handlers.OptimizeRoute(db))

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

			// Location tracking (sent every 10 seconds during active shift)
			r.Post("/driver/location", handlers.UpdateLocation(db, wsHub))

			// FCM token registration
			r.Post("/driver/fcm-token", handlers.RegisterFCMToken(db))
		})

		// Manager endpoints (require authentication + admin role)
		r.Group(func(r chi.Router) {
			r.Use(middleware.Auth)
			r.Use(middleware.RequireRole("admin"))

			r.Post("/manager/assign-route", handlers.AssignRoute(db, wsHub, fcmService))
			r.Delete("/manager/shifts/clear", handlers.ClearAllShifts(db, wsHub))

			// Fleet management
			r.Get("/manager/drivers", handlers.GetAllDrivers(db))
		})
	})

	// Get port
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Start server
	log.Printf("🚀 Server starting on http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal(err)
	}
}
