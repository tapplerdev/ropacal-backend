// Package worker runs the backend's periodic background loops.
//
// Seven loops — the move-request monitor, AirTag drift monitor, daily digest,
// GPS batch writer, AI operations agent, stale-shift monitor and placement
// refit — each hand-rolled the same loop: a ticker (or a sleep), a goroutine
// selecting on ctx.Done(), and in six of them a Stop() that sent on a stopChan.
//
// That Stop() was dead code, and dangerous dead code: nothing called it, the
// channel was unbuffered, and no loop ever read from it — so the first caller
// would have blocked forever. Shutdown has always actually run through ctx
// cancellation (see main.go's signal.NotifyContext). Periodic is the loop that
// really runs, written once, with the dead half gone.
package worker

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// Periodic describes one background loop. The zero value is not runnable:
// Name, Interval and Run are required.
type Periodic struct {
	// Name prefixes every log line and keys the loop on /health, e.g.
	// "MoveRequestMonitor".
	Name string

	// Interval between runs. The schedule is anchored when the loop starts,
	// matching the old constructor-made tickers, NOT reset after each run — a
	// slow run does not push the schedule back.
	Interval time.Duration

	// RunAtStart runs once immediately rather than waiting a full Interval for
	// the first tick. Every loop but the GPS batch writer wants this; the batch
	// writer never did, and on a 30-second tick the difference is moot.
	RunAtStart bool

	// InitialDelay holds the first run back so services the work depends on can
	// come up first (the AI agent waits two minutes). Shutdown during the delay
	// exits cleanly without ever running. Applied before RunAtStart.
	//
	// Without RunAtStart the first run is the first tick, and the ticker keeps
	// counting during the delay: a delay longer than Interval leaves a tick
	// waiting, so the first run comes straight after the delay.
	InitialDelay time.Duration

	// Run is the work. It must be safe to call repeatedly.
	//
	// A non-nil error marks the run as failed on /health; the loop carries on
	// either way. Run is expected to have logged the failure itself — the
	// per-org loop (orgdb.ForEachActiveOrg) already logs each org's failure with
	// its org id, so Periodic only counts it rather than logging it twice.
	Run func() error
}

// Start launches the loop in a goroutine registered with wg, and returns at
// once. The loop exits when ctx is cancelled — the only stop signal there is.
//
// It panics on a missing Name or Run, or a non-positive Interval, rather than
// looping hot or silently never running: those are programming errors, and
// they surface at boot.
func (p Periodic) Start(ctx context.Context, wg *sync.WaitGroup) {
	if p.Name == "" {
		panic("worker: Name is required")
	}
	if p.Interval <= 0 {
		panic(fmt.Sprintf("worker %q: Interval must be positive, got %v", p.Name, p.Interval))
	}
	if p.Run == nil {
		panic(fmt.Sprintf("worker %q: Run is nil", p.Name))
	}

	st := register(p.Name, p.Interval)

	wg.Add(1)
	go func() {
		defer wg.Done()

		// Made before any delay or startup run, so ticks land on the same
		// schedule the old constructor-made tickers produced.
		ticker := time.NewTicker(p.Interval)
		defer ticker.Stop()

		if p.InitialDelay > 0 {
			select {
			case <-ctx.Done():
				p.logStopping()
				return
			case <-time.After(p.InitialDelay):
			}
		}

		if p.RunAtStart {
			p.runSafely(st)
		}

		for {
			select {
			case <-ctx.Done():
				p.logStopping()
				return
			case <-ticker.C:
				p.runSafely(st)
			}
		}
	}()
}

// runSafely runs once and records the outcome.
//
// The recover is a backstop for code OUTSIDE the per-org loop — the AI agent's
// business-hours preamble, say. A panic inside one org's pass never reaches
// here: orgdb.ForEachActiveOrg recovers it so the other orgs still run, and
// hands it back as an error, which is how it gets counted. A panic out here,
// unrecovered, would kill the whole process, API included.
//
// Deferred cleanup inside Run (a transaction rollback, a lock release) still
// executes, because defers run during panic unwinding before recover sees it.
func (p Periodic) runSafely(st *state) {
	// Counted on ENTRY, so a run that fails still registers as attempted —
	// "runs climbing, failures climbing" is the signature of a loop that is
	// alive but broken, which is exactly what /health should surface.
	st.runs.Add(1)
	st.lastRun.Store(time.Now().Unix())
	defer func() {
		if r := recover(); r != nil {
			st.fail()
			log.Printf("❌ [%s] run panicked — recovered, loop continues: %v\n%s", p.Name, r, debug.Stack())
		}
	}()
	if err := p.Run(); err != nil {
		st.fail()
	}
}

func (p Periodic) logStopping() {
	log.Printf("🛑 [%s] Stopping...", p.Name)
}
