package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ropacal-backend/internal/orgdb"
)

// waitFor polls cond until it holds or the deadline passes. Timing-based
// assertions use generous deadlines and only ever wait for something to
// HAPPEN, never assert that it took a specific amount of time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// stopAndWait cancels the loop and proves it actually exited — a loop that
// ignores ctx would leave wg.Wait blocked and fail here.
func stopAndWait(t *testing.T, cancel context.CancelFunc, wg *sync.WaitGroup) {
	t.Helper()
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit after ctx was cancelled")
	}
}

// uniqueName keys a loop in the process-wide registry. Counters deliberately
// survive a restart under the same name, so a test that asserts exact counts
// must not share a name with its own earlier run under -count=N.
var nameSeq atomic.Int64

func uniqueName(t *testing.T) string {
	return fmt.Sprintf("%s-%d", t.Name(), nameSeq.Add(1))
}

// counting returns a Run that counts its calls and always succeeds.
func counting(n *atomic.Int32) func() error {
	return func() error { n.Add(1); return nil }
}

func TestPeriodic_RunAtStartRunsBeforeTheFirstTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int32

	// An interval so long no tick can fire during the test: any run observed
	// can only be the startup run.
	Periodic{Name: "t", Interval: time.Hour, RunAtStart: true, Run: counting(&runs)}.Start(ctx, &wg)

	waitFor(t, "startup run", func() bool { return runs.Load() == 1 })
	stopAndWait(t, cancel, &wg)
	if got := runs.Load(); got != 1 {
		t.Errorf("runs = %d, want exactly 1 (the startup run)", got)
	}
}

func TestPeriodic_WithoutRunAtStartWaitsForTheTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int32

	Periodic{Name: "t", Interval: time.Hour, Run: counting(&runs)}.Start(ctx, &wg)

	time.Sleep(30 * time.Millisecond)
	stopAndWait(t, cancel, &wg)
	if got := runs.Load(); got != 0 {
		t.Errorf("runs = %d, want 0 — nothing should run before the first tick", got)
	}
}

func TestPeriodic_RunsOnEveryTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int32

	Periodic{Name: "t", Interval: 5 * time.Millisecond, Run: counting(&runs)}.Start(ctx, &wg)

	waitFor(t, "three ticked runs", func() bool { return runs.Load() >= 3 })
	stopAndWait(t, cancel, &wg)
}

// The upper bound the lower-bound tests cannot give: a ticker never fires
// faster than its interval, so more runs than 1 + elapsed/interval means
// something runs the work twice per tick.
func TestPeriodic_RunsAtMostOncePerTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int32
	const interval = 20 * time.Millisecond

	start := time.Now()
	Periodic{Name: "t", Interval: interval, RunAtStart: true, Run: counting(&runs)}.Start(ctx, &wg)
	time.Sleep(10 * interval)
	stopAndWait(t, cancel, &wg)

	allowed := 1 + int32(time.Since(start)/interval)
	if got := runs.Load(); got > allowed {
		t.Errorf("runs = %d in %v at a %v interval, want at most %d", got, time.Since(start), interval, allowed)
	}
}

// The schedule is anchored when the loop starts, and a slow run does not push
// it back. Each run here takes most of an interval: anchored, run 2 starts one
// interval in; anchored AFTER the startup run, or re-anchored after each run,
// it would start an interval plus a run's duration in. The thresholds sit
// halfway between, so scheduler noise cannot decide the result.
func TestPeriodic_ScheduleIsAnchoredAtStartNotAtRunEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	const interval, work = 300 * time.Millisecond, 200 * time.Millisecond

	var mu sync.Mutex
	var starts []time.Duration
	start := time.Now()
	Periodic{
		Name: "t", Interval: interval, RunAtStart: true,
		Run: func() error {
			mu.Lock()
			starts = append(starts, time.Since(start))
			mu.Unlock()
			time.Sleep(work)
			return nil
		},
	}.Start(ctx, &wg)

	waitFor(t, "three runs", func() bool { mu.Lock(); defer mu.Unlock(); return len(starts) >= 3 })
	stopAndWait(t, cancel, &wg)

	mu.Lock()
	defer mu.Unlock()
	// Anchored: 0, 300ms, 600ms. Drifting: 0, 500ms, 800ms or later.
	if starts[1] >= interval+work/2 {
		t.Errorf("run 2 started at %v, want ~%v — the schedule waited for the startup run", starts[1], interval)
	}
	if starts[2] >= 2*interval+work/2 {
		t.Errorf("run 3 started at %v, want ~%v — a slow run pushed the schedule back", starts[2], 2*interval)
	}
}

// The AI agent's case: shutting down during the startup delay must exit
// without ever running the work.
func TestPeriodic_CancelDuringInitialDelayNeverRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int32

	Periodic{
		Name: "t", Interval: time.Hour, RunAtStart: true, InitialDelay: time.Hour,
		Run: counting(&runs),
	}.Start(ctx, &wg)

	stopAndWait(t, cancel, &wg)
	if got := runs.Load(); got != 0 {
		t.Errorf("runs = %d, want 0 — cancelled during the delay", got)
	}
}

func TestPeriodic_InitialDelayHoldsTheStartupRunBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int32
	start := time.Now()
	var ranAt atomic.Int64

	Periodic{
		Name: "t", Interval: time.Hour, RunAtStart: true, InitialDelay: 40 * time.Millisecond,
		Run: func() error { ranAt.Store(int64(time.Since(start))); runs.Add(1); return nil },
	}.Start(ctx, &wg)

	waitFor(t, "delayed startup run", func() bool { return runs.Load() == 1 })
	stopAndWait(t, cancel, &wg)
	if d := time.Duration(ranAt.Load()); d < 40*time.Millisecond {
		t.Errorf("startup run fired after %v, before the 40ms initial delay", d)
	}
}

// The backstop: a panic OUTSIDE the per-org loop (the AI agent's preamble, say)
// would otherwise be an unrecovered goroutine panic and kill the process. It
// must be contained to the one run, and the loop must keep going.
func TestPeriodic_PanicIsContainedAndTheLoopContinues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int32

	Periodic{
		Name: "t", Interval: 5 * time.Millisecond, RunAtStart: true,
		Run: func() error {
			if runs.Add(1) == 1 {
				panic("first run blows up")
			}
			return nil
		},
	}.Start(ctx, &wg)

	// Reaching a second run proves the panic neither killed the process (the
	// test binary would have died) nor ended the loop.
	waitFor(t, "a run after the panicking one", func() bool { return runs.Load() >= 2 })
	stopAndWait(t, cancel, &wg)
}

func TestPeriodic_RejectsAMisconfiguredLoopAtStart(t *testing.T) {
	ok := func() error { return nil }
	cases := map[string]Periodic{
		"empty name":        {Interval: time.Second, Run: ok},
		"zero interval":     {Name: "t", Run: ok},
		"negative interval": {Name: "t", Interval: -time.Second, Run: ok},
		"nil run":           {Name: "t", Interval: time.Second},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected a panic at Start, got none")
				}
			}()
			var wg sync.WaitGroup
			p.Start(context.Background(), &wg)
		})
	}
}

// Liveness has to tell a healthy loop from a broken one. Runs are counted on
// entry, so a loop that is alive but failing shows BOTH counters climbing —
// the signature /health exists to surface. Returned errors and panics both
// count.
func TestSnapshot_ReportsRunsAndFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var calls atomic.Int32
	name := uniqueName(t)

	Periodic{
		Name: name, Interval: 5 * time.Millisecond, RunAtStart: true,
		Run: func() error {
			switch calls.Add(1) % 3 {
			case 1:
				return errors.New("an org's pass failed")
			case 2:
				panic("the run itself blew up")
			}
			return nil
		},
	}.Start(ctx, &wg)

	waitFor(t, "six runs", func() bool { return calls.Load() >= 6 })
	stopAndWait(t, cancel, &wg)

	got, ok := Snapshot()[name]
	if !ok {
		t.Fatal("loop missing from Snapshot")
	}
	if got.Runs < 6 || got.Runs != int64(calls.Load()) {
		t.Errorf("Runs = %d, want every one of the %d calls", got.Runs, calls.Load())
	}
	// Two of every three runs fail: one by error, one by panic.
	if want := got.Runs - got.Runs/3; got.Failures != want {
		t.Errorf("Failures = %d of %d runs, want %d", got.Failures, got.Runs, want)
	}
	if got.LastRunUnix == 0 || got.LastFailureUnix == 0 {
		t.Errorf("timestamps not recorded: last_run=%d last_failure=%d", got.LastRunUnix, got.LastFailureUnix)
	}
	if got.IntervalSeconds != (5 * time.Millisecond).Seconds() {
		t.Errorf("IntervalSeconds = %v", got.IntervalSeconds)
	}
}

// The case the counter exists for, wired the way every worker is: one org's
// pass panics inside orgdb.ForEachActiveOrg. The per-org loop recovers it so
// the other orgs still run — and must hand it back, or /health reads healthy.
func TestSnapshot_CountsAPanicInsideThePerOrgLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int32
	name := uniqueName(t)

	Periodic{
		Name: name, Interval: time.Hour, RunAtStart: true,
		Run: func() error {
			defer runs.Add(1)
			// Tenancy dark in unit tests: one pass, with a passthrough handle.
			return orgdb.ForEachActiveOrg(nil, name, func(*orgdb.DB) error {
				var m map[string]int
				m["boom"]++ // a nil-map write, like a bad row's nil field
				return nil
			})
		},
	}.Start(ctx, &wg)

	waitFor(t, "the startup run", func() bool { return runs.Load() == 1 })
	stopAndWait(t, cancel, &wg)

	got := Snapshot()[name]
	if got.Runs != 1 || got.Failures != 1 {
		t.Errorf("runs=%d failures=%d, want 1 and 1 — the per-org panic was swallowed", got.Runs, got.Failures)
	}
}

// A loop that has not run yet must still be listed — its absence is not the
// same as "zero", and /health has to show it is waiting rather than missing.
func TestSnapshot_NeverRunLoopIsListedWithZeroes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	name := uniqueName(t)
	Periodic{Name: name, Interval: time.Hour, Run: func() error { return nil }}.Start(ctx, &wg)
	stopAndWait(t, cancel, &wg)

	got, ok := Snapshot()[name]
	if !ok {
		t.Fatal("a started loop that has not run yet is missing from Snapshot")
	}
	if got.Runs != 0 || got.Failures != 0 || got.LastRunUnix != 0 || got.LastFailureUnix != 0 {
		t.Errorf("a loop that never ticked reports %+v", got)
	}
	if got.IntervalSeconds != time.Hour.Seconds() {
		t.Errorf("IntervalSeconds = %v, want %v", got.IntervalSeconds, time.Hour.Seconds())
	}
}

// A duplicate Start must not wipe the evidence: the counters carry over, so
// the duplicate shows up as runs climbing too fast instead of vanishing.
func TestRegister_RestartUnderTheSameNameKeepsCounters(t *testing.T) {
	name := uniqueName(t)
	first := register(name, time.Minute)
	first.runs.Add(5)
	first.fail()

	again := register(name, 2*time.Minute)
	if again != first {
		t.Fatal("re-registering created a fresh state")
	}
	got := Snapshot()[name]
	if got.Runs != 5 || got.Failures != 1 {
		t.Errorf("after restart runs=%d failures=%d, want 5 and 1", got.Runs, got.Failures)
	}
	if got.IntervalSeconds != (2 * time.Minute).Seconds() {
		t.Errorf("IntervalSeconds = %v, want the restarted loop's interval", got.IntervalSeconds)
	}
}

// Snapshot is read by /health while loops register and run; under -race this
// proves the reads are synchronised with registration.
func TestSnapshot_IsSafeAlongsideRegistration(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); register("race-test", time.Duration(i+1)*time.Second) }()
		go func() { defer wg.Done(); _ = Snapshot() }()
	}
	wg.Wait()
}
