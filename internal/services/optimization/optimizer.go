package optimization

import (
	"log"
	"os"
)

// Optimizer is the interface that all route optimizers must implement
type Optimizer interface {
	OptimizeRoute(req *RouteRequest) (*RouteResponse, error)
	Name() string
}

// NewOptimizer creates an optimizer based on environment configuration
// Set OPTIMIZER_TYPE="mapbox" (only Mapbox is supported)
// Note: Google and HERE optimizers are available in code but commented out
func NewOptimizer() Optimizer {
	optimizerType := os.Getenv("OPTIMIZER_TYPE")

	log.Printf("🔧 Optimizer configuration: OPTIMIZER_TYPE=%s", optimizerType)

	switch optimizerType {
	// case "google":
	// 	// Google Route Optimization API does NOT automatically insert warehouse stops
	// 	// for capacity reload. It will skip shipments that exceed capacity.
	// 	// Not suitable for placements - use Mapbox instead.
	// 	log.Printf("✅ Using Google Route Optimization (Optimize Tours)")
	// 	return NewGoogleOptimizer()
	case "mapbox":
		log.Printf("✅ Using Mapbox Optimization v2")
		return NewMapboxOptimizer()
	// case "here":
	// 	log.Printf("✅ Using HERE Maps Optimization")
	// 	return NewHereOptimizer()
	default:
		// Default to Mapbox (only supported optimizer)
		log.Printf("⚠️  OPTIMIZER_TYPE not set or invalid, defaulting to Mapbox")
		return NewMapboxOptimizer()
	}
}

// FALLBACK OPTIMIZER DISABLED - We only use Mapbox, no fallbacks needed
//
// // NewOptimizerWithFallback creates an optimizer with fallback support
// // If the primary optimizer fails, it tries the fallback
// func NewOptimizerWithFallback() Optimizer {
// 	return &FallbackOptimizer{
// 		primary:  NewOptimizer(),
// 		fallback: NewHereOptimizer(), // Always fallback to HERE Maps
// 	}
// }

// // FallbackOptimizer wraps an optimizer with fallback support
// type FallbackOptimizer struct {
// 	primary  Optimizer
// 	fallback Optimizer
// }

// func (f *FallbackOptimizer) Name() string {
// 	return fmt.Sprintf("%s (with %s fallback)", f.primary.Name(), f.fallback.Name())
// }

// func (f *FallbackOptimizer) OptimizeRoute(req *RouteRequest) (*RouteResponse, error) {
// 	log.Printf("🔄 Attempting optimization with %s", f.primary.Name())

// 	resp, err := f.primary.OptimizeRoute(req)
// 	if err == nil {
// 		log.Printf("✅ Optimization succeeded with %s", f.primary.Name())
// 		return resp, nil
// 	}

// 	log.Printf("⚠️  Primary optimizer (%s) failed: %v", f.primary.Name(), err)
// 	log.Printf("🔄 Falling back to %s", f.fallback.Name())

// 	resp, err = f.fallback.OptimizeRoute(req)
// 	if err != nil {
// 		return nil, fmt.Errorf("both optimizers failed: primary=%s, fallback=%s: %w",
// 			f.primary.Name(), f.fallback.Name(), err)
// 	}

// 	log.Printf("✅ Optimization succeeded with fallback (%s)", f.fallback.Name())
// 	return resp, nil
// }
