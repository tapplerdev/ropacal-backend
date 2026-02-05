package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// TestMapboxOptimizationAccess tests if the account has access to Mapbox Optimization v2 API
func TestMapboxOptimizationAccess() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accessToken := os.Getenv("MAPBOX_ACCESS_TOKEN")
		if accessToken == "" {
			http.Error(w, "MAPBOX_ACCESS_TOKEN not configured", http.StatusInternalServerError)
			return
		}

		log.Printf("🧪 Testing Mapbox Optimization v2 API access...")

		// Step 1: Submit a minimal routing problem
		problem := buildMinimalTestProblem()
		jobID, err := submitMapboxProblem(accessToken, problem)
		if err != nil {
			log.Printf("❌ Failed to submit problem: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":      false,
				"error":        err.Error(),
				"message":      "Failed to submit routing problem to Mapbox",
				"has_access":   false,
				"access_token": maskToken(accessToken),
			})
			return
		}

		log.Printf("✅ Problem submitted successfully, job ID: %s", jobID)

		// Step 2: Poll for solution (max 30 seconds)
		solution, err := pollMapboxSolution(accessToken, jobID, 30*time.Second)
		if err != nil {
			log.Printf("❌ Failed to get solution: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":    false,
				"error":      err.Error(),
				"message":    "Problem submitted but failed to retrieve solution",
				"has_access": true, // Submission worked, so they have access
				"job_id":     jobID,
			})
			return
		}

		log.Printf("✅ Solution received successfully!")

		// Step 3: Parse and return results
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":    true,
			"message":    "Mapbox Optimization v2 API access confirmed!",
			"has_access": true,
			"job_id":     jobID,
			"solution":   solution,
		})
	}
}

// buildMinimalTestProblem creates a simple routing problem for testing
func buildMinimalTestProblem() map[string]interface{} {
	return map[string]interface{}{
		"version": 1,
		"locations": []map[string]interface{}{
			{
				"name":        "warehouse",
				"coordinates": []float64{-74.0060, 40.7128}, // NYC
			},
			{
				"name":        "stop-1",
				"coordinates": []float64{-74.0080, 40.7148},
			},
			{
				"name":        "stop-2",
				"coordinates": []float64{-74.0100, 40.7168},
			},
		},
		"vehicles": []map[string]interface{}{
			{
				"name":           "test-truck",
				"start_location": "warehouse",
				"end_location":   "warehouse",
			},
		},
		"services": []map[string]interface{}{
			{
				"name":     "service-1",
				"location": "stop-1",
				"duration": 300,
			},
			{
				"name":     "service-2",
				"location": "stop-2",
				"duration": 300,
			},
		},
	}
}

// submitMapboxProblem submits a routing problem to Mapbox Optimization v2 API
func submitMapboxProblem(accessToken string, problem map[string]interface{}) (string, error) {
	url := fmt.Sprintf("https://api.mapbox.com/optimized-trips/v2?access_token=%s", accessToken)

	problemJSON, err := json.Marshal(problem)
	if err != nil {
		return "", fmt.Errorf("failed to marshal problem: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(problemJSON))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Check for 401 Unauthorized (no access to beta)
	if resp.StatusCode == 401 {
		return "", fmt.Errorf("unauthorized (401): Your account does not have access to Mapbox Optimization v2 API Beta. Sign up at https://www.mapbox.com/optimization-v2")
	}

	// Check for 202 Accepted (success)
	if resp.StatusCode != 202 {
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var response struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	return response.ID, nil
}

// pollMapboxSolution polls for the solution until it's ready or timeout
func pollMapboxSolution(accessToken, jobID string, timeout time.Duration) (map[string]interface{}, error) {
	url := fmt.Sprintf("https://api.mapbox.com/optimized-trips/v2/%s?access_token=%s", jobID, accessToken)
	client := &http.Client{Timeout: 10 * time.Second}

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to check status: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		// 200 OK = solution ready
		if resp.StatusCode == 200 {
			var solution map[string]interface{}
			if err := json.Unmarshal(body, &solution); err != nil {
				return nil, fmt.Errorf("failed to parse solution: %w", err)
			}
			return solution, nil
		}

		// 202 Accepted = still processing
		if resp.StatusCode == 202 {
			log.Printf("⏳ Solution not ready yet, waiting...")
			time.Sleep(2 * time.Second)
			continue
		}

		// Other status = error
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil, fmt.Errorf("timeout waiting for solution (waited %v)", timeout)
}

// maskToken masks most of the access token for security
func maskToken(token string) string {
	if len(token) < 20 {
		return "***"
	}
	return token[:10] + "..." + token[len(token)-4:]
}
