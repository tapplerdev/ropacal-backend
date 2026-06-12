package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"ropacal-backend/internal/services/centrifugo"
)

type AIOperationsAgent struct {
	db               *sqlx.DB
	client           anthropic.Client
	fcmService       *FCMService
	centrifugoClient *centrifugo.Client
	ticker           *time.Ticker
	stopChan         chan bool
}

func NewAIOperationsAgent(db *sqlx.DB, fcmService *FCMService, centrifugoClient *centrifugo.Client) *AIOperationsAgent {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	return &AIOperationsAgent{
		db:               db,
		client:           client,
		fcmService:       fcmService,
		centrifugoClient: centrifugoClient,
		ticker:           time.NewTicker(30 * time.Minute),
		stopChan:         make(chan bool),
	}
}

func (a *AIOperationsAgent) Start() {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Println("⚠️ [AIAgent] ANTHROPIC_API_KEY not set — agent disabled")
		return
	}

	log.Println("🤖 [AIAgent] Starting AI Operations Agent (30-minute cycle)")

	go func() {
		// Run first cycle after a 2-minute delay (let other services init)
		time.Sleep(2 * time.Minute)
		a.runCycle()

		for {
			select {
			case <-a.stopChan:
				log.Println("🤖 [AIAgent] Stopping...")
				return
			case <-a.ticker.C:
				a.runCycle()
			}
		}
	}()
}

func (a *AIOperationsAgent) Stop() {
	a.ticker.Stop()
	a.stopChan <- true
}

func (a *AIOperationsAgent) runCycle() {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	hour := now.Hour()

	// Only run during business hours (6 AM - 8 PM Pacific)
	if hour < 6 || hour > 20 {
		log.Printf("🤖 [AIAgent] Outside business hours (%d:00 PT) — skipping", hour)
		return
	}

	log.Printf("🤖 [AIAgent] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("🤖 [AIAgent] Running cycle at %s", now.Format("3:04 PM"))

	// Check for critical bins
	a.checkCriticalBins()

	// Check for stale bins (not checked in 30+ days)
	a.checkStaleBins()

	// Check route performance
	a.checkRoutePerformance()

	log.Printf("🤖 [AIAgent] Cycle complete")
	log.Printf("🤖 [AIAgent] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// checkCriticalBins finds bins estimated to be at 90%+ fill with no active shift covering them
func (a *AIOperationsAgent) checkCriticalBins() {
	type criticalBin struct {
		ID              string  `db:"id"`
		BinNumber       int     `db:"bin_number"`
		CurrentStreet   string  `db:"current_street"`
		City            string  `db:"city"`
		FillPercentage  int     `db:"fill_percentage"`
		LastCheckedAt   *int64  `db:"last_checked_at"`
		AvgDailyFillRate float64
	}

	// Get bins with fill >= 80%
	var bins []criticalBin
	err := a.db.Select(&bins, `
		SELECT id, bin_number, current_street, city, fill_percentage, last_checked_at
		FROM bins WHERE status = 'active' AND fill_percentage >= 80
	`)
	if err != nil || len(bins) == 0 {
		return
	}

	// Check which bins are already on active shifts
	var coveredBinIDs []string
	a.db.Select(&coveredBinIDs, `
		SELECT DISTINCT rt.bin_id FROM route_tasks rt
		JOIN shifts s ON rt.shift_id = s.id
		WHERE s.status IN ('active', 'ready') AND rt.is_completed = 0 AND rt.is_deleted = false AND rt.bin_id IS NOT NULL
	`)
	coveredSet := map[string]bool{}
	for _, id := range coveredBinIDs {
		coveredSet[id] = true
	}

	// Filter to uncovered critical bins
	var uncovered []criticalBin
	for _, b := range bins {
		if !coveredSet[b.ID] {
			uncovered = append(uncovered, b)
		}
	}

	if len(uncovered) == 0 {
		return
	}

	log.Printf("🤖 [AIAgent] Found %d critical bins not on any active shift", len(uncovered))

	// Check for existing pending recommendations to avoid duplicates
	for _, bin := range uncovered {
		var existing int
		a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_overflow' AND status = 'pending'`, bin.ID)
		if existing > 0 {
			continue
		}

		// Calculate days since check
		daysSinceCheck := 0.0
		if bin.LastCheckedAt != nil {
			daysSinceCheck = float64(time.Now().Unix()-*bin.LastCheckedAt) / 86400
		}

		title := fmt.Sprintf("Bin #%d at %d%% fill — needs collection", bin.BinNumber, bin.FillPercentage)
		description := fmt.Sprintf("Bin #%d at %s, %s is at %d%% fill", bin.BinNumber, bin.CurrentStreet, bin.City, bin.FillPercentage)
		if daysSinceCheck > 1 {
			description += fmt.Sprintf(" and hasn't been checked in %.0f days", daysSinceCheck)
		}
		description += ". Not currently on any active shift."

		severity := "high"
		if bin.FillPercentage >= 90 {
			severity = "critical"
		}

		a.createRecommendation("bin_overflow", "bin", bin.ID, title, description, severity,
			"Add to nearest active driver's route or create emergency collection shift",
			fmt.Sprintf("Fill level at %d%% exceeds threshold. %.0f days since last check.", bin.FillPercentage, daysSinceCheck))
	}
}

// checkStaleBins finds bins not checked in 30+ days with 0% fill
func (a *AIOperationsAgent) checkStaleBins() {
	type staleBin struct {
		ID            string `db:"id"`
		BinNumber     int    `db:"bin_number"`
		CurrentStreet string `db:"current_street"`
		City          string `db:"city"`
		DaysSinceCheck float64 `db:"days_since_check"`
	}

	var bins []staleBin
	a.db.Select(&bins, `
		SELECT id, bin_number, current_street, city,
			COALESCE((EXTRACT(EPOCH FROM NOW())::BIGINT - last_checked_at)::float / 86400, 999) as days_since_check
		FROM bins
		WHERE status = 'active' AND fill_percentage <= 5
			AND (last_checked_at IS NULL OR (EXTRACT(EPOCH FROM NOW())::BIGINT - last_checked_at) > 2592000)
	`)

	for _, bin := range bins {
		var existing int
		a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_retire' AND status = 'pending'`, bin.ID)
		if existing > 0 {
			continue
		}

		title := fmt.Sprintf("Bin #%d at 0%% for %.0f days — consider retiring", bin.BinNumber, bin.DaysSinceCheck)
		description := fmt.Sprintf("Bin #%d at %s, %s has been at 0%% fill for %.0f days. This location may no longer generate donations.", bin.BinNumber, bin.CurrentStreet, bin.City, bin.DaysSinceCheck)

		a.createRecommendation("bin_retire", "bin", bin.ID, title, description, "low",
			"Retire this bin and redeploy to a higher-performing location",
			fmt.Sprintf("Bin has been at 0%% for %.0f days, suggesting the location has very low donation activity.", bin.DaysSinceCheck))
	}
}

// checkRoutePerformance checks shift history for underperforming routes
func (a *AIOperationsAgent) checkRoutePerformance() {
	type routePerf struct {
		RouteID        *string `db:"route_id"`
		AvgCompletion  float64 `db:"avg_completion"`
		ShiftCount     int     `db:"shift_count"`
	}

	var routes []routePerf
	a.db.Select(&routes, `
		SELECT route_id, AVG(completion_rate) as avg_completion, COUNT(*) as shift_count
		FROM shift_history
		WHERE route_id IS NOT NULL AND ended_at > EXTRACT(EPOCH FROM NOW())::BIGINT - 2592000
		GROUP BY route_id
		HAVING COUNT(*) >= 3 AND AVG(completion_rate) < 60
	`)

	for _, route := range routes {
		if route.RouteID == nil {
			continue
		}

		var existing int
		a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'route_split' AND status = 'pending'`, *route.RouteID)
		if existing > 0 {
			continue
		}

		// Get route name
		var routeName string
		a.db.Get(&routeName, `SELECT COALESCE(name, 'Unnamed Route') FROM routes WHERE id = $1`, *route.RouteID)

		title := fmt.Sprintf("Route '%s' averaging %.0f%% completion — consider splitting", routeName, route.AvgCompletion)
		description := fmt.Sprintf("Route '%s' has averaged %.0f%% completion rate over the last %d shifts. Drivers may not be finishing the full route.",
			routeName, route.AvgCompletion, route.ShiftCount)

		a.createRecommendation("route_split", "route", *route.RouteID, title, description, "medium",
			"Split this route into two shorter routes or remove low-priority bins",
			fmt.Sprintf("%.0f%% avg completion over %d shifts suggests the route is too long or has problematic bins.", route.AvgCompletion, route.ShiftCount))
	}
}

func (a *AIOperationsAgent) createRecommendation(recType, entityType, entityID, title, description, severity, action, reasoning string) {
	id := uuid.New().String()
	now := time.Now().Unix()
	expiresAt := now + (7 * 86400) // Expire in 7 days

	_, err := a.db.Exec(`
		INSERT INTO ai_recommendations (id, type, entity_type, entity_id, title, description, severity, recommended_action, status, source, reasoning, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', 'ai_agent', $9, $10, $11)
	`, id, recType, entityType, entityID, title, description, severity, action, reasoning, now, expiresAt)

	if err != nil {
		log.Printf("❌ [AIAgent] Failed to create recommendation: %v", err)
		return
	}

	log.Printf("🤖 [AIAgent] Created recommendation: [%s] %s", severity, title)

	// Broadcast to managers via Centrifugo
	if a.centrifugoClient != nil {
		ctx := context.Background()
		a.centrifugoClient.PublishCompanyEvent(ctx, "ai_recommendation_created", map[string]interface{}{
			"id":       id,
			"type":     recType,
			"title":    title,
			"severity": severity,
		})
	}

	// Send push notification for critical/high severity
	if (severity == "critical" || severity == "high") && a.fcmService != nil {
		var tokens []string
		a.db.Select(&tokens, `SELECT token FROM fcm_tokens WHERE user_id IN (SELECT id FROM users WHERE role = 'admin')`)
		if len(tokens) > 0 {
			a.fcmService.SendMulticast(tokens, "AI Recommendation", title, map[string]string{
				"type":              "ai_recommendation",
				"recommendation_id": id,
			})
		}
	}
}
