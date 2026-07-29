package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/jmoiron/sqlx"
)

// DiagnosticLog represents a diagnostic log from the mobile app
type DiagnosticLog struct {
	Timestamp string `json:"timestamp"`
	Context   string `json:"context"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Platform  string `json:"platform"`
}

// ReceiveDiagnosticLog handles diagnostic logs from the mobile app
// POST /api/logs/diagnostic
func ReceiveDiagnosticLog(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse request body
		var logEntry DiagnosticLog
		if err := json.NewDecoder(r.Body).Decode(&logEntry); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Log to console with color coding
		prefix := "📱"
		switch logEntry.Level {
		case "ERROR":
			prefix = "🔴"
		case "WARNING":
			prefix = "🟡"
		case "INFO":
			prefix = "🔵"
		}

		// Pretty print the diagnostic log
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("%s MOBILE DIAGNOSTIC [%s]", prefix, logEntry.Level)
		log.Printf("   Platform:  %s", logEntry.Platform)
		log.Printf("   Context:   %s", logEntry.Context)
		log.Printf("   Timestamp: %s", logEntry.Timestamp)
		log.Printf("   Message:   %s", logEntry.Message)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Send success response
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "received",
		})
	}
}
