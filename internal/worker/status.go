package worker

import (
	"sync"
	"sync/atomic"
	"time"
)

// Liveness for the background loops.
//
// Until this existed there was no way to tell, from outside the process,
// whether any of the workers was actually running. The stale-shift monitor
// ENDS live shifts and the batch writer is the only thing persisting driver GPS
// — either could have been stuck for days with nothing to show for it, and the
// Railway CLI, the only log access, is routinely logged out. Snapshot makes the
// question "is it running, and is it failing?" a single request to /health.
//
// Deliberately counts and timestamps only. A failure's error or panic value is
// never stored or exposed here: it can carry tenant data, and /health is
// unauthenticated.

type state struct {
	interval    time.Duration // guarded by mu
	runs        atomic.Int64
	failures    atomic.Int64
	lastRun     atomic.Int64 // unix seconds; 0 = never
	lastFailure atomic.Int64 // unix seconds; 0 = never
}

func (st *state) fail() {
	st.failures.Add(1)
	st.lastFailure.Store(time.Now().Unix())
}

var (
	mu       sync.Mutex
	registry = map[string]*state{}
)

// register returns the state for name, creating it on first use. Re-starting a
// loop under the same name keeps its counters rather than silently resetting
// them, so a duplicate Start shows up as a run count that climbs too fast.
func register(name string, interval time.Duration) *state {
	mu.Lock()
	defer mu.Unlock()
	st, ok := registry[name]
	if !ok {
		st = &state{}
		registry[name] = st
	}
	st.interval = interval
	return st
}

// Status is one loop's liveness, as served on /health.
type Status struct {
	IntervalSeconds float64 `json:"interval_seconds"`
	Runs            int64   `json:"runs"`
	// Failures counts runs in which anything went wrong: an org's pass errored
	// or panicked, the orgs could not be listed, or the run itself panicked.
	Failures        int64 `json:"failures"`
	LastRunUnix     int64 `json:"last_run_unix,omitempty"`
	LastFailureUnix int64 `json:"last_failure_unix,omitempty"`
}

// Snapshot reports every loop started in this process, keyed by Name.
func Snapshot() map[string]Status {
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]Status, len(registry))
	for name, st := range registry {
		out[name] = Status{
			IntervalSeconds: st.interval.Seconds(),
			Runs:            st.runs.Load(),
			Failures:        st.failures.Load(),
			LastRunUnix:     st.lastRun.Load(),
			LastFailureUnix: st.lastFailure.Load(),
		}
	}
	return out
}
