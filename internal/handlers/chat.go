package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jmoiron/sqlx"
)

const systemPrompt = `You are Binly AI, an assistant for a Bay Area waste bin management company called Binly.
You help managers understand bin performance, find locations for new bins, analyze area trends, and answer operational questions.

Key concepts:
- Bins are clothing donation bins placed at locations around the Bay Area
- Bins have fill_percentage (0-100%), checked periodically by drivers on collection shifts
- avg_daily_fill_rate = how fast a bin fills per day (higher = more demand/better location)
- Urgency levels based on estimated current fill: critical (>=80%), high (>=60%), medium (>=40%), low (<40%)
- No-go zones: areas flagged for incidents (vandalism, theft, landlord complaints). Each has a center point and radius in meters.
- Bin statuses: active (in the field), retired (permanently removed), in_storage (at warehouse), pending_move (scheduled to be relocated), missing (lost/stolen)
- Potential locations: spots suggested by drivers for placing new bins, awaiting manager review
- Shifts: drivers run collection routes, checking and emptying bins. Shifts have tasks (collection, placement, pickup, dropoff)
- Move requests: scheduled bin relocations with urgency levels

Always use the provided tools to query real data. Never guess or make up bin numbers, addresses, or statistics.
When recommending locations, always check no-go zones to ensure you don't suggest spots in restricted areas.
Use miles for all distances, not kilometers. All operations are in the San Francisco Bay Area, California.
Format responses clearly with bullet points or numbered lists when presenting multiple items.`

type ChatHandler struct {
	db     *sqlx.DB
	client anthropic.Client
	tools  []anthropic.ToolUnionParam
}

func NewChatHandler(db *sqlx.DB) *ChatHandler {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Println("⚠️ [Chat] ANTHROPIC_API_KEY not set — chat endpoint will return errors")
	}

	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	h := &ChatHandler{
		db:     db,
		client: client,
	}
	h.tools = h.buildToolDefinitions()
	return h
}

func (h *ChatHandler) buildToolDefinitions() []anthropic.ToolUnionParam {
	toolParams := []anthropic.ToolParam{
		{
			Name:        "search_bins",
			Description: anthropic.String("Search bins by city, status, fill level, days since last check, etc. Returns bin number, address, fill percentage, and status."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"city":                map[string]any{"type": "string", "description": "Filter by city name (e.g. 'San Jose', 'Fremont')"},
					"status":              map[string]any{"type": "string", "enum": []string{"active", "retired", "in_storage", "missing", "pending_move"}, "description": "Filter by bin status"},
					"min_fill_percentage": map[string]any{"type": "integer", "description": "Minimum fill percentage (0-100)"},
					"max_fill_percentage": map[string]any{"type": "integer", "description": "Maximum fill percentage (0-100)"},
					"days_since_check_min": map[string]any{"type": "integer", "description": "Only bins not checked in at least this many days"},
					"limit":               map[string]any{"type": "integer", "description": "Max results to return (default 20)"},
				},
			},
		},
		{
			Name:        "get_area_performance",
			Description: anthropic.String("Get performance metrics for geographic areas (cities or zip codes). Returns total bins, active bins, clean bins (no incidents), problematic bins (has incidents), avg fill percentage, total checks, total incidents, success rate (% clean), and composite area score."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"group_by": map[string]any{"type": "string", "enum": []string{"city", "zip"}, "description": "Group results by city or zip code (default: city)"},
					"metric":   map[string]any{"type": "string", "enum": []string{"success_rate", "fill_rate", "check_frequency", "incident_rate"}, "description": "Sort results by this metric (default: success_rate)"},
					"limit":    map[string]any{"type": "integer", "description": "Max areas to return (default 20)"},
				},
			},
		},
		{
			Name:        "get_bin_check_history",
			Description: anthropic.String("Get the check history for a specific bin — shows fill percentages over time with dates. Use bin_number (the user-facing number like #34) or bin_id (internal UUID)."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"bin_number": map[string]any{"type": "integer", "description": "The bin number (e.g. 34, 67)"},
					"bin_id":     map[string]any{"type": "string", "description": "The internal bin UUID (alternative to bin_number)"},
					"limit":      map[string]any{"type": "integer", "description": "Number of recent checks to return (default 10)"},
				},
			},
		},
		{
			Name:        "get_no_go_zones",
			Description: anthropic.String("Get no-go zones — areas flagged for incidents like vandalism, theft, or landlord complaints. Each zone has a center point, radius, and status. Use this to check if a location is near any restricted areas."),
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
			Name:        "get_shift_history",
			Description: anthropic.String("Get completed shift history. Shows shift date, driver, duration, tasks completed, distance traveled, and completion rate."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"driver_name": map[string]any{"type": "string", "description": "Filter by driver name (partial match)"},
					"days_back":   map[string]any{"type": "integer", "description": "How many days back to look (default 30)"},
					"limit":       map[string]any{"type": "integer", "description": "Max results (default 20)"},
				},
			},
		},
		{
			Name:        "get_potential_locations",
			Description: anthropic.String("Get potential locations — spots suggested by drivers for placing new bins. Shows address, who suggested it, and whether it's been converted to a bin yet."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"status": map[string]any{"type": "string", "enum": []string{"active", "converted", "all"}, "description": "Filter by status (default: active)"},
					"city":   map[string]any{"type": "string", "description": "Filter by city name"},
				},
			},
		},
		{
			Name:        "recommend_bin_locations",
			Description: anthropic.String("Generate data-driven location recommendations for placing new bins. Analyzes high-performing areas (high fill rates), finds geographic gaps between existing bins, filters out no-go zones and locations near malls/supermarkets, and scores by area demand and income level. Returns addresses with scores and reasoning."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"count":         map[string]any{"type": "integer", "description": "Number of locations to recommend (default 10, max 30)"},
					"target_city":   map[string]any{"type": "string", "description": "Optional: focus recommendations on a specific city (e.g. 'San Jose')"},
					"min_gap_miles": map[string]any{"type": "number", "description": "Minimum distance from existing bins in miles (default 0.3)"},
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
	Response       string   `json:"response"`
	ToolCallsMade  []string `json:"tool_calls_made,omitempty"`
	ConversationID string   `json:"conversation_id"`
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

	// Build conversation messages
	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(
			anthropic.NewTextBlock(req.Message),
		),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var toolCallsMade []string
	maxIterations := 5

	for i := 0; i < maxIterations; i++ {
		response, err := h.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeSonnet4_5,
			MaxTokens: 2048,
			System: []anthropic.TextBlockParam{
				{Text: systemPrompt},
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

					// Execute the tool
					result, toolErr := h.executeTool(variant.Name, variant.Input)
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

		log.Printf("✅ [Chat] Response generated (%d tool calls)", len(toolCallsMade))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatResponse{
			Response:       responseText,
			ToolCallsMade:  toolCallsMade,
			ConversationID: req.ConversationID,
		})
		return
	}

	// Max iterations reached
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
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}
