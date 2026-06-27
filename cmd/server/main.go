package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"ropacal-backend/internal/database"
	"ropacal-backend/internal/handlers"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"
	"ropacal-backend/internal/services/roads"
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

	// Load .env file (override existing env vars for local development)
	log.Println("📂 Loading environment variables...")
	// Load .env from current working directory (project root)
	if err := godotenv.Overload(".env"); err != nil {
		log.Printf("⚠️  Warning: .env file not found: %v", err)
	} else {
		log.Println("✅ .env file loaded successfully (overriding system env vars)")
	}

	// Warn (non-fatal) if third-party API keys are missing from the environment.
	// These power HERE geocoding/optimization and Mapbox route optimization; an
	// empty value means those calls fail at runtime rather than failing loudly here.
	for _, key := range []string{"HERE_API_KEY", "HERE_APP_ID", "MAPBOX_ACCESS_TOKEN"} {
		if os.Getenv(key) == "" {
			log.Printf("⚠️  %s is not set — features depending on it will fail", key)
		}
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

	// Give FCM service a DB handle for automatic dead-token cleanup
	if fcmService != nil {
		fcmService.SetDB(db)
	}

	// Initialize Centrifugo client
	log.Println("🔌 Initializing Centrifugo client...")
	centrifugoClient, err := centrifugo.NewClient()
	if err != nil {
		log.Printf("⚠️  Failed to initialize Centrifugo client: %v (real-time messaging disabled)", err)
		centrifugoClient = nil
	} else {
		log.Println("✅ Centrifugo client initialized")
	}

	// Initialize Redis client (for location caching)
	log.Println("🔌 Initializing Redis client...")
	redisClient, err := redis.NewClient()
	if err != nil {
		log.Printf("⚠️  Failed to initialize Redis client: %v (location caching disabled)", err)
		redisClient = nil
	} else {
		log.Println("✅ Redis client initialized")
	}

	// Initialize OSRM client for road snapping
	log.Println("🔌 Initializing OSRM client...")
	osrmClient := roads.NewOSRMClient()
	log.Println("✅ OSRM client initialized")

	// Start background location batch writer (writes Redis → PostgreSQL every 30s)
	if redisClient != nil {
		batchWriter := services.NewLocationBatchWriter(db, redisClient)
		batchWriter.Start()
		log.Println("✅ Location batch writer started (30-second intervals)")
	}

	// Start daily digest scheduler (sends at 8 AM & 2 PM)
	digestScheduler := services.NewDigestScheduler(db, fcmService, centrifugoClient)
	digestScheduler.Start()
	log.Println("✅ Daily digest scheduler started (hourly check, sends at 8 AM & 2 PM)")

	// Start AirTag drift monitor (checks every 5 minutes)
	airtagMonitor := services.NewAirtagMonitor(db, fcmService, centrifugoClient)
	airtagMonitor.Start()
	log.Println("✅ AirTag drift monitor started (5-minute intervals)")

	// Start move request monitor (checks for overdue/due-soon moves)
	moveRequestMonitor := services.NewMoveRequestMonitor(db, fcmService, centrifugoClient)
	moveRequestMonitor.Start()
	log.Println("✅ Move request monitor started (15-minute intervals)")

	// Start stale shift monitor (auto-ends shifts with no GPS updates)
	if redisClient != nil {
		staleShiftMonitor := services.NewStaleShiftMonitor(db, redisClient, fcmService, centrifugoClient)
		staleShiftMonitor.Start()
		log.Println("✅ Stale shift monitor started (checks for disconnected drivers)")
	}

	// Start AI Operations Agent (generates recommendations every 30 min)
	aiAgent := services.NewAIOperationsAgent(db, fcmService, centrifugoClient)
	aiAgent.Start()
	log.Println("✅ AI Operations Agent started (30-minute cycles)")

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
	// Allowed origins are configurable via the ALLOWED_ORIGINS env var
	// (comma-separated list, e.g. "https://app.example.com,https://admin.example.com").
	// Falls back to "*" (allow all) when the var is unset, preserving prior behavior.
	allowedOrigins := []string{"*"}
	if raw := os.Getenv("ALLOWED_ORIGINS"); raw != "" {
		var origins []string
		for _, o := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(o); trimmed != "" {
				origins = append(origins, trimmed)
			}
		}
		if len(origins) > 0 {
			allowedOrigins = origins
		}
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Authorization"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	// Health check. Reports a deploy marker (to confirm which build is live) and
	// booleans for whether the third-party API keys are present in the env
	// (booleans only — never the key values).
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"version": "keys-env-migration",
			"config": map[string]bool{
				"here_api_key":        os.Getenv("HERE_API_KEY") != "",
				"here_app_id":         os.Getenv("HERE_APP_ID") != "",
				"mapbox_access_token": os.Getenv("MAPBOX_ACCESS_TOKEN") != "",
			},
		})
	})

	// Authentication routes (no auth required)
	r.Post("/api/auth/login", handlers.Login(db))

	// WebSocket endpoint (authentication handled in handler via query param)
	r.Get("/ws", websocket.HandleWebSocket(wsHub, db))

	// Centrifugo proxy endpoints (called by Centrifugo server for authorization)
	r.Post("/api/centrifugo/subscribe", handlers.CentrifugoSubscribeProxy(db))
	r.Post("/api/centrifugo/publish", handlers.CentrifugoPublishProxy(db))

	// Centrifugo location publish proxy (processes GPS data before broadcast)
	r.Post("/api/centrifugo/publish-location", handlers.CentrifugoLocationPublishProxy(db, redisClient, osrmClient, centrifugoClient, fcmService))

	// Internal API routes (secured with INTERNAL_API_KEY, used by FindMy bridge)
	r.Route("/api/internal", func(r chi.Router) {
		r.Use(handlers.InternalAPIKey)
		r.Get("/airtag-accounts", handlers.GetAirtagAccounts(db))
		r.Put("/airtag-accounts/{id}/state", handlers.UpdateAirtagAccountState(db))
		r.Post("/airtag-locations", handlers.UpsertAirtagLocations(db))
	})

	// API routes
	r.Route("/api", func(r chi.Router) {
		// Geocoding endpoints (no auth required)
		r.Post("/geocoding/reverse", handlers.ReverseGeocode())
		r.Post("/geocoding/reverse/batch", handlers.BatchReverseGeocode())
		r.Post("/geocoding/forward", handlers.Geocode())
		r.Post("/geocoding/forward/batch", handlers.BatchGeocode())

		// Directions endpoint (OSRM-powered road-following polyline, no auth required)
		r.Get("/directions", handlers.GetDirections())

		// Config endpoints (warehouse location)
		r.Get("/config/warehouse", handlers.GetWarehouseLocation(db))
		r.Patch("/config/warehouse", handlers.UpdateWarehouseLocation(db, wsHub, centrifugoClient))

		// Bins endpoints (read-only - no auth required)
		r.Get("/bins", handlers.GetBins(db))
		r.Get("/bins/priority", handlers.GetBinsWithPriority(db)) // Priority sorting & filtering
		r.Get("/bins/top-performers", handlers.GetTopPerformingBins(db))

		// Bins mutation endpoints (require authentication)
		r.Group(func(r chi.Router) {
			r.Use(middleware.Auth)
			r.Post("/bins", handlers.CreateBin(db, wsHub, centrifugoClient))
			r.Patch("/bins/{id}", handlers.UpdateBin(db, wsHub, centrifugoClient))
			r.Delete("/bins/{id}", handlers.DeleteBin(db, wsHub, centrifugoClient))
			r.Post("/bins/batch-geocode", handlers.BatchGeocodeBins(db)) // Batch geocode all bins using HERE Maps
			r.Get("/bins/{id}/active-shift-dependencies", handlers.CheckBinDependencies(db, redisClient)) // Check if bin is in active shifts
		})

		// Checks endpoints
		r.Get("/bins/{id}/checks", handlers.GetChecks(db))
		r.Get("/checks", handlers.GetAllChecks(db))

		// Moves endpoints
		r.Get("/bins/{id}/moves", handlers.GetMoves(db))
		r.Post("/bins/{id}/moves", handlers.CreateMove(db))
		r.Get("/bins/{id}/move-requests", handlers.GetBinMoveRequestsByBinID(db))
		r.Get("/bins/{id}/incidents", handlers.GetBinIncidents(db))
		r.Get("/bins/{id}/change-log", handlers.GetBinChangeLog(db))

		// Route management endpoints (route blueprints/templates)
		r.Get("/routes", handlers.GetRoutes(db))
		r.Get("/routes/{id}", handlers.GetRoute(db))
		r.Post("/routes", handlers.CreateRoute(db))
		r.Post("/routes/optimize-preview", handlers.OptimizeRoutePreview(db))
		r.Post("/routes/test-here-optimization", handlers.TestHereOptimization(db))     // Testing endpoint for HERE Maps API
		r.Post("/routes/test-mapbox-optimization", handlers.TestMapboxOptimization(db)) // Testing endpoint for Mapbox API v1
		r.Patch("/routes/{id}", handlers.UpdateRoute(db))
		r.Delete("/routes/{id}", handlers.DeleteRoute(db))
		r.Post("/routes/{id}/duplicate", handlers.DuplicateRoute(db))

		// No-Go Zones endpoints
		r.Get("/no-go-zones", handlers.GetNoGoZones(db))
		r.Get("/no-go-zones/{id}", handlers.GetNoGoZone(db))
		r.Get("/no-go-zones/{id}/incidents", handlers.GetZoneIncidents(db))
		r.Get("/incidents/nearby", handlers.GetNearbyIncidents(db))

		// Shift-related incident queries
		r.Get("/shifts/{id}/incidents", handlers.GetShiftIncidents(db))

		// Analytics endpoints
		r.Get("/analytics/areas", handlers.GetAreaPerformance(db))

		// Potential Locations endpoints (managers can view all - no auth required)
		r.Get("/potential-locations", handlers.GetPotentialLocations(db))

		// Driver shift endpoints (require authentication)
		r.Group(func(r chi.Router) {
			r.Use(middleware.Auth)

			// Auth status endpoint
			r.Get("/auth/status", handlers.GetAuthStatus(db))

			// Centrifugo connection token (for real-time WebSocket)
			if centrifugoClient != nil {
				r.Get("/centrifugo/token", handlers.GetCentrifugoToken(centrifugoClient))
			}

			// Shift management
			r.Get("/driver/shift/current", handlers.GetCurrentShift(db))
			r.Post("/driver/shift/preflight", handlers.PreflightCheck(db, redisClient))
			r.Post("/driver/shift/start", handlers.StartShift(db, wsHub, redisClient, centrifugoClient))
			r.Post("/driver/shift/pause", handlers.PauseShift(db, wsHub, centrifugoClient))
			r.Post("/driver/shift/resume", handlers.ResumeShift(db, wsHub, centrifugoClient))
			r.Post("/driver/shift/end", handlers.EndShift(db, wsHub, centrifugoClient))
			r.Post("/driver/shift/complete-task", handlers.CompleteTask(db, wsHub, centrifugoClient))
			r.Post("/driver/shift/skip-task", handlers.SkipTask(db, redisClient, wsHub, centrifugoClient))

			// Shift history
			r.Get("/driver/shift-history", handlers.GetDriverShiftHistory(db))
			r.Get("/driver/shift-details", handlers.GetShiftDetails(db))
			r.Get("/driver/shift-move-requests", handlers.GetShiftMoveRequests(db))

			// Location tracking (sent every 10 seconds during active shift)
			r.Post("/driver/location", handlers.UpdateLocation(db, wsHub, redisClient, centrifugoClient))

			// OSRM test endpoint (for testing snap-to-roads integration)
			r.Post("/test/osrm", handlers.TestOSRM(db, wsHub))

			// FCM token registration
			r.Post("/driver/fcm-token", handlers.RegisterFCMToken(db))

			// Route Task endpoints (task-based shift system)
			r.Get("/shifts/{shiftId}/tasks", handlers.GetShiftTasks(db))
			r.Get("/shifts/{shiftId}/tasks/detailed", handlers.GetShiftTasksDetailed(db))
			r.Put("/shifts/tasks/{taskId}/complete", handlers.CompleteRouteTask(db, wsHub, centrifugoClient))

			// Potential Locations (drivers can create requests)
			r.Post("/potential-locations", handlers.CreatePotentialLocation(db, wsHub, centrifugoClient))

			// Incident reporting (drivers can report both check-based and field observations)
			// TODO: Implement CreateZoneIncident handler (currently handled in CompleteShiftBin)
			// r.Post("/zone-incidents", handlers.CreateZoneIncident(db))

			// Per-user notification inbox
			r.Get("/notifications", handlers.GetUserNotifications(db))
			r.Get("/notifications/unread-count", handlers.GetUnreadCount(db))
			r.Get("/notifications/preferences", handlers.GetNotificationPreferences(db))
			r.Put("/notifications/preferences", handlers.UpdateNotificationPreferences(db))
			r.Patch("/notifications/read-all", handlers.MarkAllNotificationsRead(db))
			r.Get("/notifications/{id}", handlers.GetNotificationByID(db))
			r.Patch("/notifications/{id}/read", handlers.MarkNotificationRead(db))
		})

		// Diagnostic logging endpoint (no auth required for easier debugging)
		r.Post("/logs/diagnostic", handlers.ReceiveDiagnosticLog(db))

		// App error logging endpoint (no auth required for mobile app error reporting)
		r.Post("/logs/app-error", handlers.LogAppError(db))

		// Manager endpoints (require authentication + admin role)
		r.Group(func(r chi.Router) {
			r.Use(middleware.Auth)
			r.Use(middleware.RequireRole("admin"))

			r.Post("/manager/assign-route", handlers.AssignRoute(db, wsHub, fcmService, centrifugoClient))
			r.Put("/manager/shifts/{id}/cancel", handlers.CancelShift(db, wsHub, fcmService, centrifugoClient))
			r.Post("/manager/shifts/cancel-all-active", handlers.CancelAllActiveShifts(db, wsHub, fcmService, centrifugoClient))
			r.Patch("/manager/shifts/{id}", handlers.UpdateShift(db, redisClient, centrifugoClient, fcmService)) // Comprehensive shift editing
			r.Post("/manager/shifts/{shift_id}/tasks/remove", handlers.RemoveTasksFromShift(db, redisClient, centrifugoClient, fcmService))
			r.Delete("/manager/shifts/clear", handlers.ClearAllShifts(db, wsHub, centrifugoClient))

			// Task-based shift creation (agnostic shift builder)
			r.Post("/manager/shifts/create-with-tasks", handlers.CreateShiftWithTasks(db, wsHub, centrifugoClient, fcmService))
			r.Get("/manager/shifts", handlers.GetAllShifts(db))                   // List all shifts with filtering (register first - exact match)
			r.Get("/manager/shifts/history", handlers.GetManagerShiftHistory(db))                    // Completed shift history with task stats
			r.Get("/manager/shifts/history/{shiftId}/tasks", handlers.GetShiftHistoryTasks(db))     // Per-task granular breakdown for a shift
			r.Get("/manager/shifts/{shiftId}", handlers.GetShiftByID(db))                           // Get single shift (register after)
			r.Get("/manager/shifts/{shiftId}/tasks/history", handlers.GetShiftTasksWithHistory(db)) // Get ALL tasks including deleted ones for audit trail
			r.Get("/manager/shifts/{id}/compare-optimizer", handlers.CompareOptimizerForShift(db)) // Compare Mapbox v2 optimization
			r.Get("/manager/shifts/{id}/driver-proximity", handlers.CheckShiftDriverProximity(db, redisClient)) // Check if driver is nearby current task

			// One-time data migration endpoints (can be removed after use)
			r.Post("/manager/bins/load-real", handlers.LoadRealBins(db))
			r.Post("/manager/bins/fix-status", handlers.FixBinStatus(db))

			// Bin move request management
			r.Post("/manager/bins/schedule-move", handlers.ScheduleBinMove(db, wsHub, fcmService, centrifugoClient))
r.Get("/manager/bins/move-requests", handlers.GetBinMoveRequests(db))            // List all move requests (register first - exact match)
			r.Get("/manager/bins/move-requests/{id}", handlers.GetBinMoveRequest(db))        // Get single move request (register after)
			r.Get("/manager/bins/move-requests/{id}/active-shift-dependencies", handlers.CheckMoveRequestDependencies(db)) // Check if move request is in active shifts
			r.Put("/manager/bins/move-requests/{id}", handlers.UpdateBinMoveRequest(db, redisClient, wsHub, centrifugoClient)) // Update move request
			r.Post("/manager/bins/move-requests/{id}/assign-to-shift", handlers.AssignMoveToShift(db, wsHub, fcmService, centrifugoClient))
			r.Put("/manager/bins/move-requests/{id}/cancel", handlers.CancelBinMoveRequest(db, redisClient, wsHub, centrifugoClient))
			r.Put("/manager/bins/move-requests/{id}/assign-to-user", handlers.AssignMoveToUser(db))
			r.Put("/manager/bins/move-requests/{id}/clear-assignment", handlers.ClearMoveAssignment(db))
			r.Put("/manager/bins/move-requests/{id}/complete-manually", handlers.ManuallyCompleteMoveRequest(db))
			r.Get("/manager/bins/move-requests/{id}/history", handlers.GetMoveRequestHistory(db)) // Get audit trail

			// Bin check recommendations (7-day stale bin flagging)
			r.Post("/manager/bins/flag-stale", handlers.FlagStaleBins(db))
			r.Get("/manager/bins/check-recommendations", handlers.GetBinCheckRecommendations(db))
			r.Put("/manager/bins/check-recommendations/{id}/dismiss", handlers.DismissBinCheckRecommendation(db))

			// Bin retirement & reactivation
			r.Post("/manager/bins/{id}/retire", handlers.RetireBin(db))
			r.Post("/manager/bins/{id}/reactivate", handlers.ReactivateBin(db))

			// Smart routes & daily priorities
			r.Get("/manager/bins/daily-priorities", handlers.GetDailyPriorities(db))
			r.Post("/manager/routes/generate-smart", handlers.GenerateSmartRoutes(db))
			r.Get("/manager/routes/performance", handlers.GetRoutePerformance(db))
			r.Post("/manager/routes/estimate-duration", handlers.EstimateRouteDuration(db))
			r.Post("/manager/routes/smart-reoptimize", handlers.SmartReoptimize(db))
			r.Get("/manager/bins/collection-stats", handlers.GetBinCollectionStats(db))

			// Placement Planner
			r.Get("/manager/placement/opportunities", handlers.GetPlacementOpportunities(db))

			// AI Chat
			chatHandler := handlers.NewChatHandler(db)
			r.Post("/manager/chat", chatHandler.Handle)

			// AI Recommendations
			r.Get("/manager/ai-recommendations", handlers.GetAIRecommendations(db))
			r.Get("/manager/ai-recommendations/pending-count", handlers.GetPendingRecommendationCount(db))
			r.Put("/manager/ai-recommendations/{id}/accept", handlers.AcceptRecommendation(db))
			r.Put("/manager/ai-recommendations/{id}/dismiss", handlers.DismissRecommendation(db))
			r.Put("/manager/ai-recommendations/{id}/snooze", handlers.SnoozeRecommendation(db))

			// Potential Locations management (managers can delete and convert)
			r.Get("/potential-locations/{id}/active-shift-dependencies", handlers.CheckPotentialLocationDependencies(db)) // Check if potential location is in active shifts
			r.Delete("/potential-locations/{id}", handlers.DeletePotentialLocation(db, redisClient, wsHub, centrifugoClient))
			r.Post("/potential-locations/{id}/convert", handlers.ConvertPotentialLocationToBin(db, redisClient, wsHub, centrifugoClient))
			r.Get("/bins/{binId}/nearby-potential-locations", handlers.GetNearbyPotentialLocations(db))

			// Fleet management
			r.Get("/manager/drivers", handlers.GetAllDrivers(db, redisClient))
			r.Get("/manager/drivers/{driverId}/shifts", handlers.GetDriverShiftHistoryByID(db))
			r.Get("/manager/drivers/{id}/pending-moves", handlers.GetDriverPendingMoves(db))
			r.Get("/manager/active-drivers", handlers.GetActiveDrivers(db, redisClient))
			r.Get("/manager/driver-shift-details", handlers.GetDriverShiftDetails(db))

			// Incident reporting (manager phone-call complaints → zone creation)
			r.Post("/manager/incident-report", handlers.CreateManagerIncidentReport(db, centrifugoClient))

			// User management
			r.Get("/manager/users", handlers.GetAllUsers(db))
			r.Post("/manager/users", handlers.CreateUser(db))

			// No-Go Zone management (admin only)
			r.Patch("/no-go-zones/{id}", handlers.UpdateNoGoZone(db, centrifugoClient))
			// TODO: Implement remaining admin zone management handlers
			// r.Post("/no-go-zones", handlers.CreateNoGoZone(db))
			// r.Delete("/no-go-zones/{id}", handlers.DeleteNoGoZone(db))

			// Field observations management
			r.Get("/field-observations", handlers.GetFieldObservations(db))
			r.Patch("/field-observations/{id}/verify", handlers.VerifyFieldObservation(db))

			// App error logs management (for viewing driver errors in dashboard)
			r.Get("/manager/logs/app-errors", handlers.GetAppErrorLogs(db))
			r.Get("/manager/logs/app-error-stats", handlers.GetAppErrorStats(db))
			r.Patch("/manager/logs/app-errors/{id}/resolve", handlers.ResolveAppErrorLog(db))

			// Daily digest (manual trigger)
			r.Post("/manager/daily-digest", handlers.TriggerDigest(digestScheduler))

			// Notification settings & history
			r.Get("/manager/notification-settings", handlers.GetNotificationSettings(db))
			r.Put("/manager/notification-settings", handlers.UpdateNotificationSettings(db))
			r.Get("/manager/notification-log", handlers.GetNotificationLog(db))
			r.Get("/manager/notification-log/{id}/recipients", handlers.GetNotificationRecipients(db))

			// AirTag locations (read from DB, written by FindMy bridge)
			r.Get("/manager/airtag-locations", handlers.GetAirtagLocations(db))
			r.Post("/manager/airtag-sync", handlers.SyncAirtagLocations())
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
