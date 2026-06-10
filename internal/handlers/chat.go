package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const baseSystemPrompt = `You are Binly AI, an assistant for a Bay Area waste bin management company called Binly.

## Domain Knowledge

- Bins are clothing donation bins placed at locations around the Bay Area
- fill_percentage (0-100%) — how full a bin is, checked by drivers during collection shifts
- avg_daily_fill_rate — how fast a bin fills per day. Higher = more demand = better performing location
- Urgency levels (based on estimated current fill): critical (>=80%), high (>=60%), medium (>=40%), low (<40%)
- No-go zones: areas flagged for incidents (vandalism, theft, landlord complaints). Each has a center point and radius in meters
- Bin statuses: active (in the field), retired (permanently removed), in_storage (at warehouse), pending_move (scheduled relocation), missing (lost/stolen)
- Potential locations: spots suggested by drivers for placing new bins
- Shifts: drivers run collection routes with tasks (collection, placement, pickup, dropoff)
- Move requests: scheduled bin relocations with urgency levels

## How to respond

You are a sharp ops analyst, not a consultant. Write like you're answering a colleague in Slack — direct, concise, no fluff.

1. NEVER use emojis unless the user does first.
2. NEVER use ALL CAPS headers (no "CRITICAL", "TIER 1", "MOVE IMMEDIATELY"). Use sentence case (e.g., "Tier 1 — move first").
3. Use bold and headers sparingly — not on every line. One item per line is usually enough.
4. Match response length to question complexity. "Why Palo Alto?" is 3 sentences. "Analyze all bins" can be longer.
5. Do NOT end responses with "Would you like me to..." or "Next steps:" unless the user explicitly asks for a plan or next steps.
6. Do NOT repeat the same disclaimer every response. If you've mentioned no-go zone filtering once in the conversation, don't say it again.
7. When presenting multiple items, keep each entry to one line with the key facts. Not a multi-line card with labeled fields.
8. Do NOT fabricate statistics or projections you can't back with data from the tools. If you don't have the data, say so.

Good format for recommendations:
"Tier 1 — move first
- Bin #33 (3478 Depot Rd, San Mateo) — 0% fill, 219 days since last check. Likely abandoned.
- Bin #48 (1144 Lelong St, San Jose) — 30% fill, 93 days stale. Inconsistent."

Bad format:
"## 🚨 CRITICAL - MOVE IMMEDIATELY
### **Bin #33** - 3478 Depot Rd
- **Status:** 0% fill
- **Problem:** Abandoned
- **Recommendation:** **Move immediately**"

## Data rules

1. ALWAYS use tools to query real data. NEVER guess or fabricate bin numbers, addresses, fill levels, or statistics.
2. Use miles for all distances (never kilometers). Bay Area, California.
3. When the user refers to something from earlier in the conversation, use your conversation history — do NOT ask them to repeat it.
4. When a bin's last check is >14 days old, note it as stale.

## Tool orchestration

When recommending new bin locations:
- The recommend_bin_locations tool already filters out no-go zones and malls/Safeway internally. Mention this once, don't repeat it every time.

When analyzing bin performance:
1. Use get_area_performance for the big picture
2. Then search_bins for specifics
3. Use get_bin_check_history for individual trends

When the user asks a follow-up about data you already discussed:
- Reference your prior response directly — do NOT ask the user to repeat information.`

type ChatHandler struct {
	db       *sqlx.DB
	client   anthropic.Client
	tools    []anthropic.ToolUnionParam
	sessions map[string]*chatSession
	mu       sync.RWMutex
}

type chatSession struct {
	Messages  []anthropic.MessageParam
	CreatedAt time.Time
	LastUsed  time.Time
}

func NewChatHandler(db *sqlx.DB) *ChatHandler {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Println("⚠️ [Chat] ANTHROPIC_API_KEY not set — chat endpoint will return errors")
	}

	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	h := &ChatHandler{
		db:       db,
		client:   client,
		sessions: make(map[string]*chatSession),
	}
	h.tools = h.buildToolDefinitions()

	// Clean up stale sessions every 10 minutes
	go func() {
		for {
			time.Sleep(10 * time.Minute)
			h.cleanStaleSessions()
		}
	}()

	return h
}

func (h *ChatHandler) cleanStaleSessions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, s := range h.sessions {
		if s.LastUsed.Before(cutoff) {
			delete(h.sessions, id)
		}
	}
}

// buildDynamicSystemPrompt adds real-time fleet stats to the base prompt
func (h *ChatHandler) buildDynamicSystemPrompt() string {
	type stats struct {
		TotalBins     int      `db:"total_bins"`
		ActiveBins    int      `db:"active_bins"`
		InStorage     int      `db:"in_storage"`
		Retired       int      `db:"retired"`
		AvgFill       *float64 `db:"avg_fill"`
		CriticalCount int      `db:"critical_count"`
		ZoneCount     int      `db:"zone_count"`
	}

	var s stats
	err := h.db.Get(&s, `
		SELECT
			COUNT(*) as total_bins,
			COUNT(CASE WHEN status = 'active' THEN 1 END) as active_bins,
			COUNT(CASE WHEN status = 'in_storage' THEN 1 END) as in_storage,
			COUNT(CASE WHEN status = 'retired' THEN 1 END) as retired,
			AVG(CASE WHEN status = 'active' THEN fill_percentage END) as avg_fill,
			COUNT(CASE WHEN status = 'active' AND fill_percentage >= 80 THEN 1 END) as critical_count,
			(SELECT COUNT(*) FROM no_go_zones WHERE status = 'active' AND merged_into_zone_id IS NULL) as zone_count
		FROM bins
	`)

	if err != nil {
		log.Printf("⚠️ [Chat] Failed to fetch fleet stats: %v", err)
		return baseSystemPrompt
	}

	avgFill := 0.0
	if s.AvgFill != nil {
		avgFill = *s.AvgFill
	}

	return fmt.Sprintf(`%s

## Current Fleet Status (live)
- Total bins: %d (%d active, %d in storage, %d retired)
- Average fleet fill: %.0f%%
- Critical bins (≥80%% full): %d
- Active no-go zones: %d`,
		baseSystemPrompt, s.TotalBins, s.ActiveBins, s.InStorage, s.Retired,
		avgFill, s.CriticalCount, s.ZoneCount)
}

func (h *ChatHandler) buildToolDefinitions() []anthropic.ToolUnionParam {
	toolParams := []anthropic.ToolParam{
		{
			Name: "search_bins",
			Description: anthropic.String(`Search or filter bins in the database. Use this to:
- Find all bins in a city or with a specific status
- Identify stale bins (not checked in N days) for maintenance planning
- Find bins within a fill percentage range (e.g., ≥80% for urgent pickups)
- Combine filters (e.g., "all active bins in San Jose not checked in 7+ days")
Returns: bin number, address, city, fill %, status, days since last check, coordinates.`),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"city":                 map[string]any{"type": "string", "description": "Filter by city name (e.g. 'San Jose', 'Fremont')"},
					"status":               map[string]any{"type": "string", "enum": []string{"active", "retired", "in_storage", "missing", "pending_move"}, "description": "Filter by bin status"},
					"min_fill_percentage":  map[string]any{"type": "integer", "description": "Minimum fill percentage (0-100)"},
					"max_fill_percentage":  map[string]any{"type": "integer", "description": "Maximum fill percentage (0-100)"},
					"days_since_check_min": map[string]any{"type": "integer", "description": "Only bins not checked in at least this many days"},
					"limit":                map[string]any{"type": "integer", "description": "Max results to return (default 20)"},
				},
			},
		},
		{
			Name: "get_area_performance",
			Description: anthropic.String(`Get performance metrics for geographic areas (cities or zip codes). Use this for:
- Comparing cities/areas by performance, incidents, or demand
- Finding the best areas for new bin placement (sort by fill_rate)
- Identifying problem areas (sort by incident_rate)
- Getting success rates (% of bins with zero incidents)
Returns: total bins, active bins, clean bins, problematic bins, avg fill %, total checks, total incidents, success rate, composite area score.`),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"group_by": map[string]any{"type": "string", "enum": []string{"city", "zip"}, "description": "Group results by city or zip code (default: city)"},
					"metric":   map[string]any{"type": "string", "enum": []string{"success_rate", "fill_rate", "check_frequency", "incident_rate"}, "description": "Sort results by this metric (default: success_rate)"},
					"limit":    map[string]any{"type": "integer", "description": "Max areas to return (default 20)"},
				},
			},
		},
		{
			Name: "get_bin_check_history",
			Description: anthropic.String(`Get the check history for a specific bin — shows fill percentages over time with dates and photos. Use bin_number (the user-facing number like #34) or bin_id (internal UUID). Use this to analyze fill rate trends for individual bins.`),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"bin_number": map[string]any{"type": "integer", "description": "The bin number (e.g. 34, 67)"},
					"bin_id":     map[string]any{"type": "string", "description": "The internal bin UUID (alternative to bin_number)"},
					"limit":      map[string]any{"type": "integer", "description": "Number of recent checks to return (default 10)"},
				},
			},
		},
		{
			Name: "get_no_go_zones",
			Description: anthropic.String(`Get no-go zones — areas flagged for incidents (vandalism, theft, landlord complaints). Each zone has a center point, radius, status, and incident count. Use this to:
- Check if a specific location is near any restricted areas
- List all problem areas in a city
- Verify that recommended bin locations are safe`),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"status":             map[string]any{"type": "string", "enum": []string{"active", "monitoring", "resolved", "all"}, "description": "Filter by zone status (default: active)"},
					"near_lat":           map[string]any{"type": "number", "description": "Filter zones near this latitude"},
					"near_lng":           map[string]any{"type": "number", "description": "Filter zones near this longitude"},
					"near_radius_meters": map[string]any{"type": "number", "description": "Search radius in meters for proximity filter (default 2000)"},
				},
			},
		},
		{
			Name: "get_shift_history",
			Description: anthropic.String(`Get completed shift history. Use this to analyze driver performance, shift frequency, and operational trends. Returns shift date, driver name, duration, tasks completed, distance traveled, completion rate, and end reason.`),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"driver_name": map[string]any{"type": "string", "description": "Filter by driver name (partial match)"},
					"days_back":   map[string]any{"type": "integer", "description": "How many days back to look (default 30)"},
					"limit":       map[string]any{"type": "integer", "description": "Max results (default 20)"},
				},
			},
		},
		{
			Name: "get_potential_locations",
			Description: anthropic.String(`Get potential locations — spots suggested by drivers for placing new bins. Shows address, who suggested it, date, and conversion status. Use this to review pending driver suggestions.`),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"status": map[string]any{"type": "string", "enum": []string{"active", "converted", "all"}, "description": "Filter by status (default: active)"},
					"city":   map[string]any{"type": "string", "description": "Filter by city name"},
				},
			},
		},
		{
			Name: "recommend_bin_locations",
			Description: anthropic.String(`Generate data-driven location recommendations for placing new bins. This tool:
1. Identifies high-performing areas (highest fill rates = most demand)
2. Finds geographic gaps between existing bins (underserved spots)
3. AUTOMATICALLY filters out all active no-go zones (you do NOT need to check separately)
4. AUTOMATICALLY filters out locations near malls and Safeway stores
5. Scores by area demand, gap distance, and neighborhood income level
6. Reverse geocodes each recommendation to a real street address
Always tell the user: "All locations have been verified clear of no-go zones and filtered to avoid malls/supermarkets."`),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"count":         map[string]any{"type": "integer", "description": "Number of locations to recommend (default 10, max 30)"},
					"target_city":   map[string]any{"type": "string", "description": "Optional: focus recommendations on a specific city (e.g. 'San Jose')"},
					"min_gap_miles": map[string]any{"type": "number", "description": "Minimum distance from existing bins in miles (default 0.3)"},
				},
			},
		},
		{
			Name: "get_census_income",
			Description: anthropic.String(`Look up median household income for Bay Area zip codes. Use this when the user asks about income levels, demographics, or affordability of an area. Can look up a specific zip code or return all zip codes for a city.`),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"zip":  map[string]any{"type": "string", "description": "Specific zip code to look up (e.g. '94306')"},
					"city": map[string]any{"type": "string", "description": "Look up all zip codes in a city (e.g. 'Palo Alto', 'San Jose')"},
				},
			},
		},
	}

	tools := make([]anthropic.ToolUnionParam, len(toolParams))
	for i, tp := range toolParams {
		tools[i] = anthropic.ToolUnionParam{OfTool: &tp}
	}
	return tools
}

type chatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
}

type chatResponse struct {
	Response        string           `json:"response"`
	ToolCallsMade   []string         `json:"tool_calls_made,omitempty"`
	ConversationID  string           `json:"conversation_id"`
	Recommendations json.RawMessage  `json:"recommendations,omitempty"`
}

func (h *ChatHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		http.Error(w, `{"error":"ANTHROPIC_API_KEY not configured"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("💬 [Chat] Received: %s", req.Message)

	// Load or create conversation session
	if req.ConversationID == "" {
		req.ConversationID = uuid.New().String()
	}

	h.mu.Lock()
	session, exists := h.sessions[req.ConversationID]
	if !exists {
		session = &chatSession{
			Messages:  []anthropic.MessageParam{},
			CreatedAt: time.Now(),
			LastUsed:  time.Now(),
		}
		h.sessions[req.ConversationID] = session
	}
	session.LastUsed = time.Now()

	// Append new user message
	session.Messages = append(session.Messages, anthropic.NewUserMessage(
		anthropic.NewTextBlock(req.Message),
	))

	// Keep only last 10 messages (5 exchanges) to stay within token limits.
	// Tool use conversations generate large intermediate messages (tool_use + tool_result blocks)
	// that consume most of the context window.
	if len(session.Messages) > 10 {
		session.Messages = session.Messages[len(session.Messages)-10:]
		// Ensure messages start with a user message (API requirement)
		for len(session.Messages) > 0 {
			if session.Messages[0].Role == anthropic.MessageParamRoleUser {
				break
			}
			session.Messages = session.Messages[1:]
		}
	}

	// Copy messages for this request
	messages := make([]anthropic.MessageParam, len(session.Messages))
	copy(messages, session.Messages)
	h.mu.Unlock()

	// Build dynamic system prompt with live fleet stats
	dynamicPrompt := h.buildDynamicSystemPrompt()

	ctx, cancel := context.WithTimeout(r.Context(), 240*time.Second)
	defer cancel()

	var toolCallsMade []string
	var rawRecommendations json.RawMessage
	maxIterations := 7

	for i := 0; i < maxIterations; i++ {
		response, err := h.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeSonnet4_5,
			MaxTokens: 4096,
			System: []anthropic.TextBlockParam{
				{Text: dynamicPrompt},
			},
			Messages: messages,
			Tools:    h.tools,
		})
		if err != nil {
			log.Printf("❌ [Chat] Anthropic API error: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":"AI service error: %s"}`, err.Error()), http.StatusInternalServerError)
			return
		}

		// Check if the model wants to use tools
		if response.StopReason == anthropic.StopReasonToolUse {
			// Append assistant message to conversation
			messages = append(messages, response.ToParam())

			// Process each tool call
			var toolResults []anthropic.ContentBlockParamUnion
			for _, block := range response.Content {
				switch variant := block.AsAny().(type) {
				case anthropic.ToolUseBlock:
					log.Printf("🔧 [Chat] Tool call: %s", variant.Name)
					toolCallsMade = append(toolCallsMade, variant.Name)

					result, toolErr := h.executeTool(variant.Name, variant.Input)
					// Capture structured recommendations for frontend
					if variant.Name == "recommend_bin_locations" && toolErr == nil {
						rawRecommendations = json.RawMessage(result)
					}
					if toolErr != nil {
						log.Printf("❌ [Chat] Tool error (%s): %v", variant.Name, toolErr)
						toolResults = append(toolResults, anthropic.NewToolResultBlock(
							block.ID, fmt.Sprintf("Error: %s", toolErr.Error()), true,
						))
					} else {
						toolResults = append(toolResults, anthropic.NewToolResultBlock(
							block.ID, result, false,
						))
					}
				}
			}

			// Send tool results back
			messages = append(messages, anthropic.NewUserMessage(toolResults...))
			continue
		}

		// Extract text response
		var responseText string
		for _, block := range response.Content {
			switch variant := block.AsAny().(type) {
			case anthropic.TextBlock:
				responseText += variant.Text
			}
		}

		// Save assistant response to session history
		h.mu.Lock()
		if s, ok := h.sessions[req.ConversationID]; ok {
			s.Messages = append(s.Messages, response.ToParam())
			// Cap at 20
			if len(s.Messages) > 20 {
				s.Messages = s.Messages[len(s.Messages)-20:]
			}
		}
		h.mu.Unlock()

		log.Printf("✅ [Chat] Response generated (%d tool calls, session %s)", len(toolCallsMade), req.ConversationID[:8])

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatResponse{
			Response:        responseText,
			ToolCallsMade:   toolCallsMade,
			ConversationID:  req.ConversationID,
			Recommendations: rawRecommendations,
		})
		return
	}

	http.Error(w, `{"error":"too many tool calls, please simplify your question"}`, http.StatusInternalServerError)
}

func (h *ChatHandler) executeTool(name string, input json.RawMessage) (string, error) {
	var params map[string]any
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("invalid tool input: %w", err)
	}

	switch name {
	case "search_bins":
		return h.toolSearchBins(params)
	case "get_area_performance":
		return h.toolGetAreaPerformance(params)
	case "get_bin_check_history":
		return h.toolGetBinCheckHistory(params)
	case "get_no_go_zones":
		return h.toolGetNoGoZones(params)
	case "get_shift_history":
		return h.toolGetShiftHistory(params)
	case "get_potential_locations":
		return h.toolGetPotentialLocations(params)
	case "recommend_bin_locations":
		return h.toolRecommendLocations(params)
	case "get_census_income":
		return h.toolGetCensusIncome(params)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}
