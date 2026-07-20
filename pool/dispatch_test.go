package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

// newHealthTestPool builds a pool with accounts whose IDs are given, so tests
// can assert dispatch ordering without relying on config persistence.
func newHealthTestPool(ids ...string) *AccountPool {
	accounts := make([]config.Account, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, config.Account{ID: id})
	}
	return &AccountPool{
		accounts:     accounts,
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		runtimeStats: make(map[string]*accountRuntimeStats),
	}
}

// TestSmartSchedulerPrefersLowerInFlight proves the health-aware tie-break:
// among otherwise-equal accounts, the one with fewer in-flight dispatches is
// selected. Two equal candidates: selecting the first (RecordSuccess finishes
// it), then selecting again must pick a different account because the first
// still has in-flight=0 but the second has been picked once (recentSelected).
func TestSmartSchedulerPrefersLowerInFlight(t *testing.T) {
	p := newHealthTestPool("a", "b")
	now := time.Now()

	// Pretend "a" already has one in-flight dispatch.
	p.markAccountSelectedLocked("a", now)

	acc := p.GetNextExcluding(nil)
	if acc == nil {
		t.Fatal("expected an account")
	}
	if acc.ID != "b" {
		t.Fatalf("smart scheduler should prefer lower-in-flight b over a, got %q", acc.ID)
	}
}

// TestRecordSuccessDecrementsInFlight proves finishing a dispatch releases the
// in-flight slot, so the account becomes selectable again on equal footing.
func TestRecordSuccessDecrementsInFlight(t *testing.T) {
	p := newHealthTestPool("only")
	p.markAccountSelectedLocked("only", time.Now())

	snap := p.GetRuntimeStatsSnapshot()
	if snap["only"].InFlight != 1 {
		t.Fatalf("expected inFlight=1 after selection, got %d", snap["only"].InFlight)
	}

	p.RecordSuccess("only")

	snap = p.GetRuntimeStatsSnapshot()
	if snap["only"].InFlight != 0 {
		t.Fatalf("expected inFlight=0 after RecordSuccess, got %d", snap["only"].InFlight)
	}
	if snap["only"].SuccessCount != 1 {
		t.Fatalf("expected successCount=1, got %d", snap["only"].SuccessCount)
	}
}

// TestRecordPermanentRejectionIsNeutral proves a permanent upstream rejection
// releases the in-flight slot WITHOUT recording a success or failure: the
// account's health must not be penalised for a bad request it merely relayed.
func TestRecordPermanentRejectionIsNeutral(t *testing.T) {
	p := newHealthTestPool("acc")
	p.markAccountSelectedLocked("acc", time.Now())

	p.RecordPermanentRejection("acc")

	snap := p.GetRuntimeStatsSnapshot()
	if snap["acc"].InFlight != 0 {
		t.Fatalf("expected inFlight=0 after permanent rejection, got %d", snap["acc"].InFlight)
	}
	if snap["acc"].SuccessCount != 0 {
		t.Fatalf("permanent rejection must not count as success, got %d", snap["acc"].SuccessCount)
	}
	if snap["acc"].FailureCount != 0 {
		t.Fatalf("permanent rejection must not count as failure, got %d", snap["acc"].FailureCount)
	}
}

// TestInFlightClampedAtZero proves a double-finish (a code path bug, or
// RecordError called without a matching selection) can never drive in-flight
// negative. This is the safety guarantee that prevents a leaked finish from
// corrupting the scheduler.
func TestInFlightClampedAtZero(t *testing.T) {
	p := newHealthTestPool("acc")
	// Finish twice without any selection — must not panic and must clamp at 0.
	p.RecordError("acc", false)
	p.RecordError("acc", false)

	snap := p.GetRuntimeStatsSnapshot()
	if snap["acc"].InFlight != 0 {
		t.Fatalf("expected inFlight clamped at 0, got %d", snap["acc"].InFlight)
	}
}

// TestDisableAccountReleasesInFlight proves disabling an account that holds an
// in-flight slot releases it, so the runtimeStats stays accurate across a
// disable/re-enable lifecycle. config must be initialised first because
// DisableAccount persists via config.SetAccountBanStatus.
func TestDisableAccountReleasesInFlight(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newHealthTestPool("acc")
	p.markAccountSelectedLocked("acc", time.Now())
	if snap := p.GetRuntimeStatsSnapshot()["acc"]; snap.InFlight != 1 {
		t.Fatalf("expected inFlight=1 before disable, got %d", snap.InFlight)
	}

	p.DisableAccount("acc", "test")

	if snap, ok := p.GetRuntimeStatsSnapshot()["acc"]; ok && snap.InFlight != 0 {
		t.Fatalf("expected inFlight=0 after disable, got %d", snap.InFlight)
	}
}

// TestLeastCooldownFallbackPreserved proves the repo's availability behaviour
// (return the earliest-cooldown account when all are cooling down, rather than
// nil/503) survived the selection rewrite.
func TestLeastCooldownFallbackPreserved(t *testing.T) {
	p := newHealthTestPool("soon", "later")
	now := time.Now()
	p.cooldowns["soon"] = now.Add(1 * time.Second)
	p.cooldowns["later"] = now.Add(1 * time.Hour)

	acc := p.GetNextExcluding(nil)
	if acc == nil {
		t.Fatal("expected least-cooldown fallback, got nil")
	}
	if acc.ID != "soon" {
		t.Fatalf("expected earliest-cooldown account 'soon', got %q", acc.ID)
	}
}

// TestFallbackSkipsNearExpiredTokenAccount proves that when all main-sweep
// candidates are filtered out, the fallback loop does not fast-return an
// account whose token is near expiry (ExpiresAt within the skew window)
// over a cooling but token-valid account. Previously the else-branch
// (no cooldown entry) returned immediately without checking token expiry,
// so a token-expired account could win over a cooling valid-token one.
func TestFallbackSkipsNearExpiredTokenAccount(t *testing.T) {
	now := time.Now()
	// "fresh" has a valid token but is on a 5-second cooldown.
	// "stale" has no cooldown but its token expires in 30s (within the 120s skew).
	fresh := config.Account{ID: "fresh", ExpiresAt: now.Unix() + 600}
	stale := config.Account{ID: "stale", ExpiresAt: now.Unix() + 30}
	p := &AccountPool{
		accounts:     []config.Account{stale, fresh}, // stale listed first so it wins without the fix
		cooldowns:    map[string]time.Time{"fresh": now.Add(5 * time.Second)},
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		runtimeStats: make(map[string]*accountRuntimeStats),
	}

	// All accounts fail the main sweep (fresh is cooling; stale has near-expired token).
	acc := p.GetNextExcluding(nil)
	if acc == nil {
		t.Fatal("expected an account from fallback, got nil")
	}
	// The fallback must prefer "fresh" (cooling but token valid) over "stale"
	// (no cooldown but token near-expired). Without the fix, "stale" is returned.
	if acc.ID != "fresh" {
		t.Fatalf("fallback must prefer cooling+token-valid 'fresh' over no-cooldown+near-expired 'stale', got %q", acc.ID)
	}
}

// TestFallbackReturnsLastResortWhenNoTokenValidCooling proves that when every
// account either has a near-expired token or no available token, the fallback
// still returns a last-resort (no-cooldown, near-expired) account rather than
// nil, so availability is preserved and the handler can refresh the token.
func TestFallbackReturnsLastResortWhenNoTokenValidCooling(t *testing.T) {
	now := time.Now()
	// Both accounts have near-expired tokens; neither has a cooldown.
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", ExpiresAt: now.Unix() + 30},
			{ID: "b", ExpiresAt: now.Unix() + 30},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		runtimeStats: make(map[string]*accountRuntimeStats),
	}

	acc := p.GetNextExcluding(nil)
	if acc == nil {
		t.Fatal("expected a last-resort account when all tokens are near-expired, got nil")
	}
}

// TestSelectionSpansBothAccounts proves that across many selections without
// finishing, the scheduler does not starve one account entirely (round-robin
// fairness via recentSelectedCount tie-break once in-flight ties).
func TestSelectionSpansBothAccounts(t *testing.T) {
	p := newHealthTestPool("x", "y")
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		acc := p.GetNextExcluding(nil)
		if acc != nil {
			seen[acc.ID] = true
		}
	}
	// Without finishing, both accounts should eventually be selected as the
	// recentSelectedCount tie-break rotates the choice.
	if !seen["x"] || !seen["y"] {
		t.Fatalf("expected both accounts selected across 50 picks, got %v", seen)
	}
}
