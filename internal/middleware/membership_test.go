package middleware

import (
	"testing"
	"time"
)

func resetMembershipCache() {
	membershipMu.Lock()
	membershipCache = make(map[string]membershipEntry)
	membershipMu.Unlock()
}

// The cache is the whole performance argument for this check (4 round trips per
// miss on Railway RTT), so assert it actually caches within the TTL.
func TestMembershipCacheWindow(t *testing.T) {
	resetMembershipCache()
	key := "u1|o1"
	membershipStore(key, membershipEntry{role: "admin", member: true, verified: time.Now()})

	membershipMu.Lock()
	e, ok := membershipCache[key]
	membershipMu.Unlock()
	if !ok || !e.member || e.role != "admin" {
		t.Fatalf("entry not stored: %+v ok=%v", e, ok)
	}
	if time.Since(e.verified) >= membershipTTL {
		t.Fatal("a just-stored entry must be inside the fresh window")
	}
}

// Fail-closed is the point of the item; an unbounded stale window would
// silently restore the 7-day exposure during any sustained DB problem.
func TestStaleCapIsBounded(t *testing.T) {
	if membershipStaleCap <= membershipTTL {
		t.Fatal("the stale cap must exceed the fresh TTL or it can never be used")
	}
	if membershipStaleCap > 10*time.Minute {
		t.Fatalf("stale cap %s is too generous to call this fail-closed", membershipStaleCap)
	}
}

// Overflow must not grow without bound.
func TestCacheEvictsOnOverflow(t *testing.T) {
	resetMembershipCache()
	old := time.Now().Add(-2 * membershipStaleCap)
	for i := 0; i < membershipCacheMax; i++ {
		membershipStore(string(rune(i%1000))+"|old"+time.Duration(i).String(),
			membershipEntry{member: true, verified: old})
	}
	membershipStore("fresh|org", membershipEntry{member: true, verified: time.Now()})

	membershipMu.Lock()
	n := len(membershipCache)
	membershipMu.Unlock()
	if n > membershipCacheMax {
		t.Fatalf("cache grew past its cap: %d entries", n)
	}
	if _, ok := membershipCache["fresh|org"]; !ok {
		t.Fatal("the newest entry must survive eviction")
	}
}
