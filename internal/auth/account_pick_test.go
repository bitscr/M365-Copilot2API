package auth

import (
	"testing"
)

// Next used to advance an in-memory cursor, so the first requests after every
// restart were served by the accounts at the head of accounts.json while the
// tail sat idle. Selection is now a random start offset.

// TestNextIsNotSequential: a store whose picker always returns 0 would still be
// sequential, so drive it with a fixed non-zero offset and assert the walk
// proceeds from there — and that a different offset yields a different account.
func TestNextIsNotSequential(t *testing.T) {
	s := &Store{data: Cache{Accounts: []AccountToken{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}}}

	// start at 2 -> expect c, then a fresh draw for the next call
	s.pick = func(int) int { return 2 }
	got, ok := s.Next()
	if !ok {
		t.Fatal("Next: no account")
	}
	if got.ID != "c" {
		t.Fatalf("start offset 2 must yield c, got %q", got.ID)
	}

	s.pick = func(int) int { return 0 }
	got, _ = s.Next()
	if got.ID != "a" {
		t.Fatalf("start offset 0 must yield a, got %q", got.ID)
	}

	// The whole point: with the production picker the draw must not be pinned
	// to the head. Sample and require more than one distinct account.
	s.pick = nil
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		acc, ok := s.Next()
		if !ok {
			t.Fatal("Next: no account")
		}
		seen[acc.ID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("random selection must reach every account, saw %d/4", len(seen))
	}
}

// TestNextRandomDrawSpreadsLoad: a head-biased selector would put ~50% of
// draws on one account. With random selection over 4 accounts, the busiest
// should stay well under that.
func TestNextRandomDrawSpreadsLoad(t *testing.T) {
	s := &Store{data: Cache{Accounts: []AccountToken{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}}}
	counts := map[string]int{}
	const draws = 4000
	for i := 0; i < draws; i++ {
		acc, _ := s.Next()
		counts[acc.ID]++
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if counts[id] == 0 {
			t.Fatalf("account %q never drawn: %v", id, counts)
		}
		// Random over 4 accounts has real variance; a 40% cap is far above
		// the expected 25% and far below the 50% a head-biased cursor gives.
		if float64(counts[id])/draws > 0.40 {
			t.Fatalf("account %q took %.1f%% of draws — head bias is back: %v",
				id, 100*float64(counts[id])/draws, counts)
		}
	}
}

// TestNextSkipsIneligible: random start must not weaken eligibility. Every
// account in the pool being ineligible still yields no account, and the scan
// wraps past them to find an eligible one wherever the draw lands.
func TestNextSkipsIneligible(t *testing.T) {
	s := &Store{data: Cache{Accounts: []AccountToken{
		{ID: "a", Status: "disabled"},
		{ID: "b"},
		{ID: "c", ScheduleDisabled: true},
		{ID: "d", Status: "expired"},
		{ID: "e"},
	}}}
	for start := 0; start < 5; start++ {
		s.pick = func(int) int { return start }
		acc, ok := s.Next()
		if !ok {
			t.Fatalf("start %d: eligible accounts exist, got none", start)
		}
		if acc.ID != "b" && acc.ID != "e" {
			t.Fatalf("start %d: ineligible account %q selected", start, acc.ID)
		}
	}

	// Everything ineligible -> no account, not a random ineligible one.
	s.data.Accounts = []AccountToken{{ID: "x", Status: "disabled"}}
	if _, ok := s.Next(); ok {
		t.Fatal("all-ineligible pool must report no account")
	}
}

// TestNextEmptyPool guards the n==0 branch: rand.IntN(0) would panic, so the
// empty check has to come first.
func TestNextEmptyPool(t *testing.T) {
	s := &Store{}
	if _, ok := s.Next(); ok {
		t.Fatal("empty store must report no account")
	}
	// pick must not be consulted at all when n == 0.
	called := false
	s.pick = func(int) int { called = true; return 0 }
	if _, ok := s.Next(); ok {
		t.Fatal("empty store must report no account")
	}
	if called {
		t.Fatal("picker must not be called for an empty pool")
	}
}
