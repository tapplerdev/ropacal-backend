package roads

import (
	"log"
	"os"
	"strings"
)

// SnapProvider represents the road snapping provider to use
type SnapProvider string

const (
	// ProviderGoogle uses Google Roads API (default, paid)
	ProviderGoogle SnapProvider = "google"

	// ProviderOSRM uses OSRM Map Matching (free, open source)
	ProviderOSRM SnapProvider = "osrm"
)

// NewRoadSnapperFromEnv creates a RoadSnapper based on environment configuration
// Reads SNAP_PROVIDER environment variable:
//   - "google" -> Google Roads API (default)
//   - "osrm"   -> OSRM Map Matching
func NewRoadSnapperFromEnv() RoadSnapper {
	provider := os.Getenv("SNAP_PROVIDER")

	switch strings.ToLower(provider) {
	case "osrm":
		log.Printf("🛣️  Using OSRM Map Matching for road snapping")
		return NewOSRMClient()

	case "google", "":
		// Default to Google for backward compatibility
		log.Printf("🛣️  Using Google Roads API for road snapping")
		return NewRoadsClient()

	default:
		log.Printf("⚠️  Unknown SNAP_PROVIDER '%s', defaulting to Google Roads API", provider)
		return NewRoadsClient()
	}
}

// NewRoadSnapper creates a RoadSnapper with explicit provider selection
func NewRoadSnapper(provider SnapProvider) RoadSnapper {
	switch provider {
	case ProviderOSRM:
		log.Printf("🛣️  Creating OSRM Map Matching client")
		return NewOSRMClient()

	case ProviderGoogle:
		log.Printf("🛣️  Creating Google Roads API client")
		return NewRoadsClient()

	default:
		log.Printf("⚠️  Unknown provider '%s', defaulting to Google Roads API", provider)
		return NewRoadsClient()
	}
}
