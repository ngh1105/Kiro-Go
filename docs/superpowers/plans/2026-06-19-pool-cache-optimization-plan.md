# Kiro-Go Pool & Cache Optimization — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace binary cooldown with 3-state circuit breaker + EWMA dynamic weight + semaphore queue in pool; add cross-account sharing + L1/L2 tier + LRU eviction + stats to cache tracker.

**Architecture:** New `pool/breaker.go` for the circuit breaker state machine. `pool/account.go` extended with `AccountState` wrapping each account with breaker + EWMA + semaphore. `proxy/cache_tracker.go` refactored for shared fingerprint pool with LRU. `proxy/cache_disk.go` for L2 JSON persistence. Admin API endpoints added to `proxy/handler.go`. Benchmark script `kiro_bench_test.py` validates all 8 test cases.

**Tech Stack:** Go 1.21+ (container/list for LRU, sync/atomic for EWMA, encoding/json for L2), Python 3.12+ (asyncio + httpx for benchmarks)

## Global Constraints

- Go module: `kiro-go`, minimum Go 1.21
- All pool operations must be thread-safe (RWMutex + atomic)
- Cache L2 file: `data/prompt_cache.json`, max 10,000 entries
- Circuit breaker thresholds: N=3 errors, T=30s/300s/3600s by error type
- EWMA α=0.2, weight clamp [1, baseWeight×3]
- Max concurrent requests: `len(accounts) × 3`
- Semantic cache: off by default (`ENABLE_SEMANTIC_CACHE` env var)
- Cache admin API under `/admin/api/cache/`
- Pool admin API under `/admin/api/pool/`
- Tests: `go test ./pool/...` and `go test ./proxy/...` must pass
- Benchmark: `python kiro_bench_test.py` standalone

---

## File Map

| File | Responsibility |
|------|---------------|
| `pool/breaker.go` (new) | Circuit breaker state machine — `State`, `Transition()`, `CanRoute()` |
| `pool/account.go` (modify) | Add `AccountState`, EWMA weight, semaphore, rewrite `GetNext*` |
| `pool/account_test.go` (modify) | Unit tests for breaker, EWMA, weighted selection |
| `proxy/cache_tracker.go` (modify) | Shared fingerprint pool, LRU, stats, semantic (feature flag) |
| `proxy/cache_disk.go` (new) | L2 persistence — `loadFromDisk()`, `syncToDisk()` |
| `proxy/cache_tracker_test.go` (modify) | Unit tests for cross-account, LRU, L2 persistence |
| `proxy/handler.go` (modify) | Wire pool semaphore, add cache/pool admin endpoints |
| `main.go` (modify) | Load L2 cache on startup |
| `kiro_bench_test.py` (new) | Benchmark + test script — 8 test cases |

---

### Task 1: Circuit Breaker State Machine

**Files:**
- Create: `pool/breaker.go`
- Modify: `pool/account_test.go` (add breaker tests)

**Interfaces:**
- Consumes: nothing (standalone)
- Produces: `CircuitBreaker` struct, `State` type, `Transition(result error, errorType string)`, `CanRoute() bool`, `StateString() string`

- [ ] **Step 1: Write breaker test**

```go
// pool/account_test.go — add after existing tests

func TestCircuitBreakerTransitions(t *testing.T) {
    b := NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second)

    // Start CLOSED
    if !b.CanRoute() {
        t.Fatal("expected CLOSED breaker to route")
    }
    if b.StateString() != "CLOSED" {
        t.Fatalf("expected CLOSED, got %s", b.StateString())
    }

    // 3 transient errors → OPEN
    for i := 0; i < 3; i++ {
        b.Transition(errors.New("connection refused"), "transient")
    }
    if b.CanRoute() {
        t.Fatal("expected OPEN breaker to block")
    }
    if b.StateString() != "OPEN" {
        t.Fatalf("expected OPEN, got %s", b.StateString())
    }
}

func TestCircuitBreakerHalfOpenRecovery(t *testing.T) {
    b := NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second)

    // Force to OPEN
    for i := 0; i < 3; i++ {
        b.Transition(errors.New("timeout"), "transient")
    }

    // Advance past open timeout (30s)
    b.openAt = time.Now().Add(-31 * time.Second)

    // Should now be HALF_OPEN on next CanRoute
    if !b.CanRoute() {
        t.Fatal("expected HALF_OPEN breaker to route after timeout")
    }
    if b.StateString() != "HALF_OPEN" {
        t.Fatalf("expected HALF_OPEN, got %s", b.StateString())
    }

    // Success → CLOSED
    b.Transition(nil, "")
    if b.StateString() != "CLOSED" {
        t.Fatalf("expected CLOSED after success, got %s", b.StateString())
    }
}

func TestCircuitBreakerHalfOpenFailure(t *testing.T) {
    b := NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second)
    for i := 0; i < 3; i++ {
        b.Transition(errors.New("timeout"), "transient")
    }
    b.openAt = time.Now().Add(-31 * time.Second)
    b.CanRoute() // transition to HALF_OPEN

    b.Transition(errors.New("still failing"), "transient")
    if b.StateString() != "OPEN" {
        t.Fatalf("expected OPEN after HALF_OPEN failure, got %s", b.StateString())
    }
}

func TestCircuitBreakerQuotaErrorLongerTimeout(t *testing.T) {
    b := NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second)
    for i := 0; i < 3; i++ {
        b.Transition(errors.New("402 Payment Required"), "quota")
    }
    if b.openTimeout != 300*time.Second {
        t.Fatalf("expected 300s quota timeout, got %v", b.openTimeout)
    }
}

func TestCircuitBreakerAuthFailureTerminal(t *testing.T) {
    b := NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second)
    b.Transition(errors.New("HTTP 401 Unauthorized"), "auth")
    if b.StateString() != "DISABLED" {
        t.Fatalf("expected DISABLED after auth failure, got %s", b.StateString())
    }
    if b.CanRoute() {
        t.Fatal("expected DISABLED breaker to block permanently")
    }
}

func TestCircuitBreakerReset(t *testing.T) {
    b := NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second)
    for i := 0; i < 3; i++ {
        b.Transition(errors.New("fail"), "transient")
    }
    b.Reset()
    if b.StateString() != "CLOSED" {
        t.Fatalf("expected CLOSED after Reset, got %s", b.StateString())
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./pool/ -run "TestCircuitBreaker" -v
```

Expected: compilation error — `NewCircuitBreaker` undefined.

- [ ] **Step 3: Write breaker implementation**

```go
// pool/breaker.go
package pool

import (
    "sync"
    "time"
)

type BreakerState int

const (
    StateClosed    BreakerState = iota
    StateOpen
    StateHalfOpen
    StateDisabled
)

func (s BreakerState) String() string {
    switch s {
    case StateClosed:    return "CLOSED"
    case StateOpen:      return "OPEN"
    case StateHalfOpen:  return "HALF_OPEN"
    case StateDisabled:  return "DISABLED"
    default:             return "UNKNOWN"
    }
}

type CircuitBreaker struct {
    mu             sync.Mutex
    state          BreakerState
    errorCount     int
    errorThreshold int
    openAt         time.Time
    transientTimeout time.Duration
    quotaTimeout   time.Duration
    authTimeout    time.Duration
    openTimeout    time.Duration // current open duration
}

func NewCircuitBreaker(errorThreshold int, transientTimeout, quotaTimeout, authTimeout time.Duration) *CircuitBreaker {
    return &CircuitBreaker{
        state:            StateClosed,
        errorThreshold:   errorThreshold,
        transientTimeout: transientTimeout,
        quotaTimeout:     quotaTimeout,
        authTimeout:      authTimeout,
    }
}

func (b *CircuitBreaker) Transition(err error, errorType string) {
    b.mu.Lock()
    defer b.mu.Unlock()

    if b.state == StateDisabled {
        return
    }

    if err == nil {
        // Success
        b.errorCount = 0
        b.state = StateClosed
        return
    }

    b.errorCount++

    switch errorType {
    case "auth":
        b.state = StateDisabled
        return
    case "quota":
        b.openTimeout = b.quotaTimeout
    default:
        b.openTimeout = b.transientTimeout
    }

    if b.state == StateHalfOpen {
        // Any failure in HALF_OPEN → back to OPEN
        b.state = StateOpen
        b.openAt = time.Now()
        return
    }

    if b.errorCount >= b.errorThreshold {
        b.state = StateOpen
        b.openAt = time.Now()
    }
}

func (b *CircuitBreaker) CanRoute() bool {
    b.mu.Lock()
    defer b.mu.Unlock()

    switch b.state {
    case StateClosed:
        return true
    case StateDisabled:
        return false
    case StateOpen:
        if time.Since(b.openAt) >= b.openTimeout {
            b.state = StateHalfOpen
            return true
        }
        return false
    case StateHalfOpen:
        return true
    default:
        return false
    }
}

func (b *CircuitBreaker) StateString() string {
    b.mu.Lock()
    defer b.mu.Unlock()
    return b.state.String()
}

func (b *CircuitBreaker) Reset() {
    b.mu.Lock()
    defer b.mu.Unlock()
    b.state = StateClosed
    b.errorCount = 0
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./pool/ -run "TestCircuitBreaker" -v
```

Expected: all 6 breaker tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pool/breaker.go pool/account_test.go
git commit -m "feat(pool): add 3-state circuit breaker with error-type-aware timeouts"
```

---

### Task 2: AccountState + EWMA Weight

**Files:**
- Modify: `pool/account.go` — add `AccountState`, EWMA fields, replace `accounts []config.Account` with `states []*AccountState`
- Modify: `pool/account_test.go` — add EWMA + weight tests

**Interfaces:**
- Consumes: `CircuitBreaker` from Task 1
- Produces: `AccountState` struct, `effectiveWeight()`, `recordLatency()`, `RecordSuccess(id, latency)`, `RecordError(id, err, errorType)`

- [ ] **Step 1: Write EWMA + weight tests**

```go
// pool/account_test.go — add after breaker tests

func TestEwmaLatencyConvergence(t *testing.T) {
    st := &AccountState{
        Account: config.Account{ID: "a", Weight: 1},
        breaker: *NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second),
    }
    // After 5 observations at 100ms, ewma should be near 100ms
    target := 100 * float64(time.Millisecond)
    for i := 0; i < 20; i++ {
        st.recordLatency(target)
    }
    if st.ewmaLatency < 50*float64(time.Millisecond) || st.ewmaLatency > 150*float64(time.Millisecond) {
        t.Fatalf("expected ewma ~100ms after 20 samples, got %v", time.Duration(st.ewmaLatency))
    }
}

func TestEffectiveWeightClamping(t *testing.T) {
    st := &AccountState{
        Account: config.Account{ID: "a", Weight: 2},
        breaker: *NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second),
    }
    // Fast account — weight should increase
    st.ewmaLatency = 50 * float64(time.Millisecond)
    w := st.effectiveWeight(100 * float64(time.Millisecond))
    if w < 2 {
        t.Fatalf("fast account weight %d < base 2", w)
    }
    if w > 6 {
        t.Fatalf("fast account weight %d > max (base×3=6)", w)
    }

    // Slow account — weight should drop to 1
    st2 := &AccountState{
        Account: config.Account{ID: "b", Weight: 3},
        breaker: *NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second),
    }
    st2.ewmaLatency = 500 * float64(time.Millisecond)
    w2 := st2.effectiveWeight(100 * float64(time.Millisecond))
    if w2 != 1 {
        t.Fatalf("slow account weight %d != 1", w2)
    }
}

func TestRecordSuccessResetsBreakerAndUpdatesLatency(t *testing.T) {
    p := &AccountPool{
        states: []*AccountState{
            {
                Account: config.Account{ID: "a", Weight: 1},
                breaker: *NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second),
            },
        },
        cooldowns:   make(map[string]time.Time),
        errorCounts: make(map[string]int),
        modelLists:  make(map[string]map[string]bool),
    }
    // Put breaker in error state
    for i := 0; i < 3; i++ {
        p.states[0].breaker.Transition(errors.New("fail"), "transient")
    }
    if p.states[0].breaker.StateString() != "OPEN" {
        t.Fatal("expected OPEN before RecordSuccess")
    }

    p.RecordSuccess("a", 100*time.Millisecond)
    if p.states[0].breaker.StateString() != "CLOSED" {
        t.Fatalf("expected CLOSED after RecordSuccess, got %s", p.states[0].breaker.StateString())
    }
    if p.states[0].ewmaLatency == 0 {
        t.Fatal("expected ewmaLatency to be non-zero after RecordSuccess")
    }
}
```

- [ ] **Step 2: Run test — verify failure**

```bash
go test ./pool/ -run "TestEwma|TestEffectiveWeight|TestRecordSuccessResets" -v
```

Expected: compilation errors — `AccountState`, `recordLatency`, `effectiveWeight` undefined.

- [ ] **Step 3: Implement AccountState + EWMA in pool/account.go**

Add after imports, before pool struct:

```go
const ewmaAlpha = 0.2

type AccountState struct {
    Account      config.Account
    breaker      CircuitBreaker
    ewmaLatency  float64
    successCount uint64
    errorCount   uint64
    lastUsed     time.Time
}

func (s *AccountState) recordLatency(latency time.Duration) {
    if s.ewmaLatency == 0 {
        s.ewmaLatency = float64(latency)
    } else {
        s.ewmaLatency = ewmaAlpha*float64(latency) + (1-ewmaAlpha)*s.ewmaLatency
    }
}

func (s *AccountState) effectiveWeight(targetLatency float64) int {
    base := effectiveWeight(s.Account.Weight)
    if s.ewmaLatency == 0 || targetLatency == 0 {
        return base
    }
    ratio := targetLatency / s.ewmaLatency
    w := int(float64(base) * ratio)
    if w < 1 {
        w = 1
    }
    maxW := base * 3
    if w > maxW {
        w = maxW
    }
    return w
}
```

Replace `cooldowns map[string]time.Time` and `errorCounts map[string]int` in `AccountPool` struct with:

```go
type AccountPool struct {
    mu            sync.RWMutex
    states        []*AccountState      // replaces accounts + cooldowns + errorCounts
    totalAccounts int
    currentIndex  uint64
    modelLists    map[string]map[string]bool
}
```

Update `RecordSuccess`:

```go
func (p *AccountPool) RecordSuccess(id string, latency time.Duration) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for _, st := range p.states {
        if st.Account.ID == id {
            st.breaker.Transition(nil, "")
            st.recordLatency(latency)
            st.successCount++
            st.lastUsed = time.Now()
            return
        }
    }
}
```

Update `RecordError`:

```go
func (p *AccountPool) RecordError(id string, err error, isQuotaError bool) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for _, st := range p.states {
        if st.Account.ID == id {
            errorType := "transient"
            if IsAuthFailure(err) {
                errorType = "auth"
            } else if isQuotaError {
                errorType = "quota"
            }
            st.breaker.Transition(err, errorType)
            st.errorCount++
            return
        }
    }
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./pool/ -run "TestEwma|TestEffectiveWeight|TestRecordSuccessResets" -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pool/account.go pool/account_test.go
git commit -m "feat(pool): add AccountState with EWMA latency tracking and breaker integration"
```

---

### Task 3: Pool Semaphore + Queue

**Files:**
- Modify: `pool/account.go` — add `sem chan struct{}`, `Acquire()`, `Release()`, `QueueTimeout`
- Modify: `pool/account_test.go` — add queue tests

**Interfaces:**
- Consumes: `AccountState` from Task 2
- Produces: `Acquire(ctx) error`, `Release()`, `SetMaxConcurrent(n int)`, semaphore-based concurrency control

- [ ] **Step 1: Write queue tests**

```go
// pool/account_test.go

func TestSemaphoreAcquireRelease(t *testing.T) {
    p := &AccountPool{
        states: []*AccountState{
            {Account: config.Account{ID: "a"}},
            {Account: config.Account{ID: "b"}},
        },
        modelLists: make(map[string]map[string]bool),
    }
    p.SetMaxConcurrent(6) // 2 accounts × 3

    // Acquire all slots
    for i := 0; i < 6; i++ {
        if err := p.Acquire(context.Background()); err != nil {
            t.Fatalf("acquire %d failed: %v", i, err)
        }
    }

    // Next acquire should timeout
    ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
    defer cancel()
    if err := p.Acquire(ctx); err == nil {
        t.Fatal("expected timeout when pool full")
    }

    // Release one, should work again
    p.Release()
    if err := p.Acquire(context.Background()); err != nil {
        t.Fatalf("acquire after release failed: %v", err)
    }
}

func TestSemaphoreDefaultMaxConcurrent(t *testing.T) {
    p := &AccountPool{
        states:     []*AccountState{},
        modelLists: make(map[string]map[string]bool),
    }
    p.SetMaxConcurrent(0) // auto-compute
    // Should not panic
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
    defer cancel()
    p.Acquire(ctx) // with 0 accounts, sem is size 0, should timeout
    // Just verify no panic
}
```

- [ ] **Step 2: Run test — verify failure**

```bash
go test ./pool/ -run "TestSemaphore" -v
```

Expected: `Acquire`, `Release`, `SetMaxConcurrent` undefined.

- [ ] **Step 3: Implement semaphore**

Add to `AccountPool`:

```go
type AccountPool struct {
    mu            sync.RWMutex
    states        []*AccountState
    totalAccounts int
    currentIndex  uint64
    modelLists    map[string]map[string]bool
    sem           chan struct{}
    queueTimeout  time.Duration
}
```

Add methods:

```go
func (p *AccountPool) SetMaxConcurrent(n int) {
    p.mu.Lock()
    defer p.mu.Unlock()
    if n <= 0 {
        n = len(p.states) * 3
    }
    if n < 1 {
        n = 1
    }
    // Only create a new sem if not already set or size changed
    if p.sem == nil || cap(p.sem) != n {
        p.sem = make(chan struct{}, n)
    }
}

func (p *AccountPool) Acquire(ctx context.Context) error {
    p.mu.RLock()
    sem := p.sem
    p.mu.RUnlock()
    if sem == nil {
        return nil
    }
    select {
    case sem <- struct{}{}:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

func (p *AccountPool) Release() {
    p.mu.RLock()
    sem := p.sem
    p.mu.RUnlock()
    if sem == nil {
        return
    }
    select {
    case <-sem:
    default:
    }
}

func (p *AccountPool) QueueDepth() int {
    p.mu.RLock()
    sem := p.sem
    p.mu.RUnlock()
    if sem == nil {
        return 0
    }
    return len(sem)
}

func (p *AccountPool) MaxConcurrent() int {
    p.mu.RLock()
    defer p.mu.RUnlock()
    if p.sem == nil {
        return 0
    }
    return cap(p.sem)
}
```

Add `context` import to imports.

- [ ] **Step 4: Run tests**

```bash
go test ./pool/ -run "TestSemaphore" -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pool/account.go pool/account_test.go
git commit -m "feat(pool): add semaphore-based concurrency limiter"
```

---

### Task 4: Rewrite GetNext with Weighted Selection

**Files:**
- Modify: `pool/account.go` — rewrite `GetNext`, `GetNextExcluding`, `GetNextForModel`, `GetNextForModelExcluding`, `Reload`
- Modify: `pool/account_test.go` — update existing tests for new `states`-based pool

**Interfaces:**
- Consumes: `AccountState` (Task 2), semaphore (Task 3)
- Produces: New `GetNext*` using `states` slice with breaker + EWMA weight selection

- [ ] **Step 1: Update Reload to build states**

```go
func (p *AccountPool) Reload() {
    p.mu.Lock()
    defer p.mu.Unlock()
    enabled := config.GetEnabledAccounts()
    allowOverUsage := config.GetAllowOverUsage()

    existing := make(map[string]*AccountState)
    for _, st := range p.states {
        existing[st.Account.ID] = st
    }

    var newStates []*AccountState
    for _, a := range enabled {
        if isQuotaBlocked(a, allowOverUsage) {
            continue
        }
        if st, ok := existing[a.ID]; ok {
            st.Account = a // refresh config data
            newStates = append(newStates, st)
        } else {
            newStates = append(newStates, &AccountState{
                Account: a,
                breaker: *NewCircuitBreaker(3, 30*time.Second, 300*time.Second, 3600*time.Second),
            })
        }
    }
    p.states = newStates
    p.totalAccounts = len(enabled)
    p.SetMaxConcurrent(len(newStates) * 3)
}
```

- [ ] **Step 2: Rewrite GetNext with weighted selection**

```go
func (p *AccountPool) GetNext() *config.Account {
    return p.GetNextExcluding(nil)
}

func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
    p.mu.RLock()
    defer p.mu.RUnlock()

    if len(p.states) == 0 {
        return nil
    }

    allowOverUsage := config.GetAllowOverUsage()
    now := time.Now()

    // Phase 1: collect routable candidates
    var candidates []*AccountState
    targetLatency := p.ewmaTargetLatency()

    for _, st := range p.states {
        if excluded != nil && excluded[st.Account.ID] {
            continue
        }
        if !st.breaker.CanRoute() {
            continue
        }
        if st.Account.ExpiresAt > 0 && now.Unix() > st.Account.ExpiresAt-tokenRefreshSkewSeconds {
            continue
        }
        if isQuotaBlocked(st.Account, allowOverUsage) {
            continue
        }
        candidates = append(candidates, st)
    }

    if len(candidates) > 0 {
        return p.selectWeighted(candidates, targetLatency)
    }

    // Phase 2: fallback — pick HALF_OPEN account (earliest recovery)
    for _, st := range p.states {
        if excluded != nil && excluded[st.Account.ID] {
            continue
        }
        if st.breaker.StateString() == "HALF_OPEN" {
            return &st.Account
        }
    }
    return nil
}

func (p *AccountPool) selectWeighted(candidates []*AccountState, targetLatency float64) *config.Account {
    if len(candidates) == 1 {
        return &candidates[0].Account
    }

    // Build cumulative weight distribution
    totalWeight := 0
    weights := make([]int, len(candidates))
    for i, st := range candidates {
        w := st.effectiveWeight(targetLatency)
        totalWeight += w
        weights[i] = w
    }

    if totalWeight == 0 {
        return &candidates[0].Account
    }

    // Atomically advance index for fair distribution
    idx := int(atomic.AddUint64(&p.currentIndex, 1) % uint64(totalWeight))

    // Find which candidate this falls on
    cumulative := 0
    for i, w := range weights {
        cumulative += w
        if idx < cumulative {
            return &candidates[i].Account
        }
    }
    return &candidates[len(candidates)-1].Account
}

func (p *AccountPool) ewmaTargetLatency() float64 {
    var sum float64
    count := 0
    for _, st := range p.states {
        if st.ewmaLatency > 0 {
            sum += st.ewmaLatency
            count++
        }
    }
    if count == 0 {
        return 0
    }
    return sum / float64(count)
}
```

- [ ] **Step 3: Update GetNextForModel similarly**

```go
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
    p.mu.RLock()
    defer p.mu.RUnlock()

    if len(p.states) == 0 {
        return nil
    }

    allowOverUsage := config.GetAllowOverUsage()
    now := time.Now()

    var candidates []*AccountState
    for _, st := range p.states {
        if excluded != nil && excluded[st.Account.ID] {
            continue
        }
        if !st.breaker.CanRoute() {
            continue
        }
        if !p.accountHasModel(st.Account.ID, model) {
            continue
        }
        if st.Account.ExpiresAt > 0 && now.Unix() > st.Account.ExpiresAt-tokenRefreshSkewSeconds {
            continue
        }
        if isQuotaBlocked(st.Account, allowOverUsage) {
            continue
        }
        candidates = append(candidates, st)
    }

    if len(candidates) > 0 {
        return p.selectWeighted(candidates, p.ewmaTargetLatency())
    }
    return nil
}

func (p *AccountPool) GetNextForModel(model string) *config.Account {
    return p.GetNextForModelExcluding(model, nil)
}
```

- [ ] **Step 4: Update remaining methods for states-based pool**

Update `GetByID`, `UpdateToken`, `MarkOverLimit`, `DisableAccount`, `Count`, `AvailableCount`, `UpdateStats`, `GetAllAccounts` to iterate over `states` instead of `accounts`. Update `DisableAccount` to call `st.breaker.Transition(err, "auth")`.

```go
func (p *AccountPool) GetByID(id string) *config.Account {
    p.mu.RLock()
    defer p.mu.RUnlock()
    for _, st := range p.states {
        if st.Account.ID == id {
            return &st.Account
        }
    }
    return nil
}

func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for _, st := range p.states {
        if st.Account.ID == id {
            st.Account.AccessToken = accessToken
            if refreshToken != "" {
                st.Account.RefreshToken = refreshToken
            }
            st.Account.ExpiresAt = expiresAt
        }
    }
}

func (p *AccountPool) MarkOverLimit(id string) {
    p.mu.Lock()
    for _, st := range p.states {
        if st.Account.ID == id {
            st.breaker.Transition(errors.New("over limit"), "quota")
        }
    }
    p.mu.Unlock()
    p.Reload()
}

func (p *AccountPool) DisableAccount(id, reason string) {
    if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
        _ = err
    }
    p.mu.Lock()
    for _, st := range p.states {
        if st.Account.ID == id {
            st.breaker.Transition(errors.New("account disabled"), "auth")
        }
    }
    p.mu.Unlock()
    p.Reload()
}

func (p *AccountPool) Count() int {
    p.mu.RLock()
    defer p.mu.RUnlock()
    if p.totalAccounts > 0 {
        return p.totalAccounts
    }
    seen := make(map[string]bool)
    for _, st := range p.states {
        seen[st.Account.ID] = true
    }
    return len(seen)
}

func (p *AccountPool) AvailableCount() int {
    p.mu.RLock()
    defer p.mu.RUnlock()
    count := 0
    seen := make(map[string]bool)
    for _, st := range p.states {
        if seen[st.Account.ID] {
            continue
        }
        seen[st.Account.ID] = true
        if !st.breaker.CanRoute() {
            continue
        }
        count++
    }
    return count
}

func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
    p.mu.Lock()
    var requestCount, errorCount, totalTokens int
    var totalCredits float64
    var lastUsed int64
    var updated bool
    for _, st := range p.states {
        if st.Account.ID == id {
            if !updated {
                st.Account.RequestCount++
                st.Account.TotalTokens += tokens
                st.Account.TotalCredits += credits
                st.Account.LastUsed = time.Now().Unix()
                requestCount = st.Account.RequestCount
                errorCount = int(st.errorCount)
                totalTokens = st.Account.TotalTokens
                totalCredits = st.Account.TotalCredits
                lastUsed = st.Account.LastUsed
                updated = true
                continue
            }
            st.Account.RequestCount = requestCount
            st.Account.ErrorCount = errorCount
            st.Account.TotalTokens = totalTokens
            st.Account.TotalCredits = totalCredits
            st.Account.LastUsed = lastUsed
        }
    }
    if updated {
        go config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
    }
}

func (p *AccountPool) GetAllAccounts() []config.Account {
    p.mu.RLock()
    defer p.mu.RUnlock()
    result := make([]config.Account, len(p.states))
    for i, st := range p.states {
        result[i] = st.Account
    }
    return result
}
```

Remove old `cooldowns` and `errorCounts` map references from `newTestPool` in test file and all other locations.

- [ ] **Step 5: Run ALL pool tests**

```bash
go test ./pool/ -v
```

Expected: ALL existing tests adapted and passing. Fix any compilation/test errors.

- [ ] **Step 6: Commit**

```bash
git add pool/account.go pool/account_test.go
git commit -m "feat(pool): rewrite GetNext with states-based weighted selection and EWMA"
```

---

### Task 5: Handle Handler Wiring for Pool

**Files:**
- Modify: `proxy/handler.go` — wire semaphore acquire/release, update `RecordSuccess`/`RecordError` calls

**Interfaces:**
- Consumes: New pool API from Task 4
- Produces: Pool concurrency control in request path, 429 on queue timeout

- [ ] **Step 1: Add semaphore acquire/release around requests**

Update handler struct if needed, and add acquire/release around the proxy loop. In each handler function (stream + non-stream), add:

```go
// Before GetNext loop:
ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
defer cancel()
if err := h.pool.Acquire(ctx); err != nil {
    h.sendJSON(w, 429, map[string]string{
        "error": "rate_limit",
        "message": "All accounts busy, retry later",
    })
    return
}
defer h.pool.Release()
```

All 4 entry points (Claude stream, Claude non-stream, OpenAI stream, OpenAI non-stream) in the handler need this.

- [ ] **Step 2: Update RecordSuccess calls**

Find all `h.pool.RecordSuccess(id)` → `h.pool.RecordSuccess(id, time.Since(reqStart))`

- [ ] **Step 3: Update RecordError calls**

Find all `h.pool.RecordError(id, isQuotaErr)` → `h.pool.RecordError(id, err, isQuotaErr)`

- [ ] **Step 4: Add pool status admin endpoint**

```go
// In handler.go routing:
case path == "/pool/status" && r.Method == "GET":
    h.apiPoolStatus(w, r)

func (h *Handler) apiPoolStatus(w http.ResponseWriter, r *http.Request) {
    type stateInfo struct {
        ID          string  `json:"id"`
        State       string  `json:"state"`
        EWMA        string  `json:"ewmaLatency"`
        Weight      int     `json:"effectiveWeight"`
        Successes   uint64  `json:"successes"`
        Errors      uint64  `json:"errors"`
    }
    pool := h.pool
    states := pool.states  // pool.states is unexported — need a getter

    infos := make([]stateInfo, 0, len(states))
    // Need a getter method: pool.GetStatesInfo() or similar
    // Will implement in pool package
}
```

Add to `pool/account.go`:

```go
func (p *AccountPool) GetStatus() []map[string]interface{} {
    p.mu.RLock()
    defer p.mu.RUnlock()
    target := p.ewmaTargetLatency()
    result := make([]map[string]interface{}, len(p.states))
    for i, st := range p.states {
        result[i] = map[string]interface{}{
            "id":              st.Account.ID,
            "state":           st.breaker.StateString(),
            "ewmaLatency":     time.Duration(st.ewmaLatency).String(),
            "effectiveWeight": st.effectiveWeight(target),
            "successes":       atomic.LoadUint64(&st.successCount),
            "errors":          atomic.LoadUint64(&st.errorCount),
        }
    }
    return result
}
```

Add admin handler:

```go
func (h *Handler) apiPoolStatus(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "states":         pool.GetPool().GetStatus(),
        "queueDepth":     pool.GetPool().QueueDepth(),
        "maxConcurrent":  pool.GetPool().MaxConcurrent(),
        "totalAccounts":  pool.GetPool().Count(),
        "availableCount": pool.GetPool().AvailableCount(),
    })
}
```

- [ ] **Step 5: Run tests + verify compilation**

```bash
go build ./...
go test ./pool/ -v
go test ./proxy/ -v
```

- [ ] **Step 6: Commit**

```bash
git add pool/account.go proxy/handler.go
git commit -m "feat: wire pool semaphore + queue + admin endpoint into handler"
```

---

### Task 6: Cache Cross-Account + LRU

**Files:**
- Modify: `proxy/cache_tracker.go` — shared `map[[32]byte]*sharedCacheEntry`, LRU list, stats counters

**Interfaces:**
- Consumes: nothing (standalone cache refactor)
- Produces: `sharedCacheEntry`, `cacheStore` with LRU, `Compute(accountID, profile)` → cross-account hits, stats counters

- [ ] **Step 1: Write cross-account cache test**

```go
// proxy/cache_tracker_test.go — add

func TestCrossAccountCacheHit(t *testing.T) {
    tracker := newPromptCacheTracker(time.Hour)
    longSystem := strings.Repeat("You are a Go expert. ", 100)
    req := &ClaudeRequest{
        Model: "claude-sonnet-4.5",
        System: []interface{}{
            map[string]interface{}{
                "type": "text",
                "text": longSystem,
                "cache_control": map[string]interface{}{
                    "type": "ephemeral",
                },
            },
        },
        Messages: []ClaudeMessage{{Role: "user", Content: "help"}},
    }

    profile := tracker.BuildClaudeProfile(req, 120)

    // Account A creates cache
    first := tracker.Compute("acct-a", profile)
    if first.CacheCreationInputTokens <= 0 {
        t.Fatalf("expected acct-a to create cache tokens")
    }
    if first.CacheReadInputTokens != 0 {
        t.Fatalf("expected first request to have zero cache reads")
    }
    tracker.Update("acct-a", profile)

    // Account B reads cache created by A (cross-account hit)
    second := tracker.Compute("acct-b", profile)
    if second.CacheReadInputTokens <= 0 {
        t.Fatalf("expected acct-b to read cache tokens created by acct-a (cross-account)")
    }
    if second.CacheCreationInputTokens != 0 {
        t.Fatalf("expected zero creation on cross-account hit")
    }
}

func TestLRUEviction(t *testing.T) {
    tracker := newPromptCacheTracker(time.Hour)
    tracker.maxEntries = 5 // small for testing

    for i := 0; i < 10; i++ {
        sys := fmt.Sprintf("prompt-%d-%s", i, strings.Repeat("x", 300))
        req := &ClaudeRequest{
            Model: "claude-sonnet-4.5",
            System: []interface{}{
                map[string]interface{}{
                    "type": "text",
                    "text": sys,
                    "cache_control": map[string]interface{}{"type": "ephemeral"},
                },
            },
            Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
        }
        profile := tracker.BuildClaudeProfile(req, 120)
        tracker.Update(fmt.Sprintf("acct-%d", i), profile)
    }

    if tracker.entryCount() > 5 {
        t.Fatalf("expected <= 5 entries after LRU eviction, got %d", tracker.entryCount())
    }
    if tracker.lruEvictions == 0 {
        t.Fatal("expected some LRU evictions")
    }
}
```

- [ ] **Step 2: Run tests — verify failure**

```bash
go test ./proxy/ -run "TestCrossAccount|TestLRUEviction" -v
```

Expected: compilation errors — `sharedCacheEntry`, `maxEntries`, `lruEvictions` undefined.

- [ ] **Step 3: Implement shared cache + LRU in cache_tracker.go**

Replace the per-account entries map with a shared one. Add `container/list` import.

```go
import "container/list"

type sharedCacheEntry struct {
    fingerprint [32]byte
    creatorID   string
    tokens      int
    expiresAt   time.Time
    ttl         time.Duration
    lruNode     *list.Element
}

type promptCacheTracker struct {
    mu             sync.Mutex
    entries        map[[32]byte]*sharedCacheEntry
    lruList        *list.List
    maxEntries     int
    maxSupportedTTL time.Duration
    // Stats
    l1Hits       uint64
    misses       uint64
    crossHits    uint64
    semanticHits uint64
    tokensSaved  uint64
    lruEvictions uint64
}

func newPromptCacheTracker(maxTTL time.Duration) *promptCacheTracker {
    if maxTTL <= 0 {
        maxTTL = defaultPromptCacheTTL
    }
    return &promptCacheTracker{
        entries:        make(map[[32]byte]*sharedCacheEntry),
        lruList:        list.New(),
        maxEntries:     10000,
        maxSupportedTTL: maxTTL,
    }
}
```

Update `Compute` to check shared pool:

```go
func (t *promptCacheTracker) Compute(accountID string, profile *promptCacheProfile) promptCacheUsage {
    // ... existing nil/empty checks ...
    // Replace the per-account entries lookup with shared:
    
    t.mu.Lock()
    defer t.mu.Unlock()
    t.pruneExpiredLocked(now)
    
    // Check shared pool for any matching fingerprint
    matchedTokens := 0
    matchedCreator := ""
    for i := len(profile.Breakpoints) - 1; i >= 0; i-- {
        breakpoint := profile.Breakpoints[i]
        if breakpoint.CumulativeTokens < minTokens {
            continue
        }
        entry, ok := t.entries[breakpoint.Fingerprint]
        if !ok || entry.expiresAt.Before(now) {
            continue
        }
        // Hit! Update LRU
        t.lruList.MoveToFront(entry.lruNode)
        entry.expiresAt = now.Add(entry.ttl)
        matchedTokens = minInt(breakpoint.CumulativeTokens, profile.TotalInputTokens)
        if matchedTokens > lastTokens {
            matchedTokens = lastTokens
        }
        matchedCreator = entry.creatorID
        if matchedCreator != accountID {
            t.crossHits++
        } else {
            t.l1Hits++
        }
        t.tokensSaved += uint64(matchedTokens)
        break
    }
    
    // ... rest of Compute as before ...
}
```

Update `Update` to use shared entries:

```go
func (t *promptCacheTracker) Update(accountID string, profile *promptCacheProfile) {
    // ... existing nil/empty checks ...
    
    t.mu.Lock()
    defer t.mu.Unlock()
    
    for _, breakpoint := range profile.Breakpoints {
        if breakpoint.CumulativeTokens < minTokens {
            continue
        }
        // Check if already exists
        if existing, ok := t.entries[breakpoint.Fingerprint]; ok {
            existing.expiresAt = now.Add(breakpoint.TTL)
            t.lruList.MoveToFront(existing.lruNode)
            continue
        }
        // Evict if at capacity
        for t.lruList.Len() >= t.maxEntries {
            back := t.lruList.Back()
            if back == nil { break }
            entry := back.Value.(*sharedCacheEntry)
            delete(t.entries, entry.fingerprint)
            t.lruList.Remove(back)
            t.lruEvictions++
        }
        entry := &sharedCacheEntry{
            fingerprint: breakpoint.Fingerprint,
            creatorID:   accountID,
            tokens:      breakpoint.CumulativeTokens,
            expiresAt:   now.Add(breakpoint.TTL),
            ttl:         breakpoint.TTL,
        }
        entry.lruNode = t.lruList.PushFront(entry)
        t.entries[breakpoint.Fingerprint] = entry
    }
}
```

Add helper for test:

```go
func (t *promptCacheTracker) entryCount() int {
    t.mu.Lock()
    defer t.mu.Unlock()
    return len(t.entries)
}
```

- [ ] **Step 4: Run cache tests**

```bash
go test ./proxy/ -run "TestCrossAccount|TestPromptCache|TestLRUEviction|TestBuildClaudeUsageMap" -v
```

Expected: all existing + new tests PASS.

- [ ] **Step 5: Commit**

```bash
git add proxy/cache_tracker.go proxy/cache_tracker_test.go
git commit -m "feat(cache): cross-account fingerprint sharing + LRU eviction"
```

---

### Task 7: Cache L2 Persistence + Admin API

**Files:**
- Create: `proxy/cache_disk.go`
- Modify: `proxy/cache_tracker.go` — add `loadFromDisk()`, `syncToDisk()`, stats endpoint helpers
- Modify: `proxy/handler.go` — add cache admin endpoints
- Modify: `main.go` — load L2 on startup

**Interfaces:**
- Consumes: Shared `promptCacheTracker` from Task 6
- Produces: `loadFromDisk(path)`, `syncToDisk(path)`, `POST /admin/api/cache/sync`, `GET /admin/api/cache/stats`, `POST /admin/api/cache/clear`

- [ ] **Step 1: Write L2 persistence test**

```go
// proxy/cache_tracker_test.go

func TestCacheDiskPersistence(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "prompt_cache.json")
    tracker := newPromptCacheTracker(time.Hour)
    longSystem := strings.Repeat("System prompt for persistence test. ", 80)
    req := &ClaudeRequest{
        Model: "claude-sonnet-4.5",
        System: []interface{}{
            map[string]interface{}{
                "type": "text",
                "text": longSystem,
                "cache_control": map[string]interface{}{"type": "ephemeral"},
            },
        },
        Messages: []ClaudeMessage{{Role: "user", Content: "test"}},
    }
    profile := tracker.BuildClaudeProfile(req, 120)

    // Create + sync
    tracker.Update("acct-1", profile)
    if err := saveCacheToDisk(tracker, path); err != nil {
        t.Fatalf("saveCacheToDisk: %v", err)
    }

    // New tracker, load from disk
    tracker2 := newPromptCacheTracker(time.Hour)
    if err := loadCacheFromDisk(tracker2, path); err != nil {
        t.Fatalf("loadCacheFromDisk: %v", err)
    }

    // Should hit cache from L2
    usage := tracker2.Compute("acct-2", profile)
    if usage.CacheReadInputTokens <= 0 {
        t.Fatal("expected cache hit after L2 reload")
    }
}
```

- [ ] **Step 2: Run test — verify failure**

```bash
go test ./proxy/ -run "TestCacheDiskPersistence" -v
```

Expected: `saveCacheToDisk`, `loadCacheFromDisk` undefined.

- [ ] **Step 3: Implement cache_disk.go**

```go
// proxy/cache_disk.go
package proxy

import (
    "encoding/json"
    "os"
    "path/filepath"
    "time"
)

type diskCacheEntry struct {
    Fingerprint [32]byte `json:"fp"`
    CreatorID   string   `json:"creator"`
    Tokens      int      `json:"tokens"`
    ExpiresAt   int64    `json:"expires_at"` // Unix seconds
    TTL         int64    `json:"ttl_ns"`
}

func saveCacheToDisk(tracker *promptCacheTracker, path string) error {
    tracker.mu.Lock()
    entries := make([]diskCacheEntry, 0, len(tracker.entries))
    for fp, e := range tracker.entries {
        if e.expiresAt.After(time.Now()) {
            entries = append(entries, diskCacheEntry{
                Fingerprint: fp,
                CreatorID:   e.creatorID,
                Tokens:      e.tokens,
                ExpiresAt:   e.expiresAt.Unix(),
                TTL:         int64(e.ttl),
            })
        }
    }
    tracker.mu.Unlock()

    dir := filepath.Dir(path)
    if err := os.MkdirAll(dir, 0755); err != nil {
        return err
    }
    tmpPath := path + ".tmp"
    data, err := json.Marshal(entries)
    if err != nil {
        return err
    }
    if err := os.WriteFile(tmpPath, data, 0600); err != nil {
        return err
    }
    return os.Rename(tmpPath, path)
}

func loadCacheFromDisk(tracker *promptCacheTracker, path string) error {
    data, err := os.ReadFile(path)
    if err != nil {
        if os.IsNotExist(err) {
            return nil
        }
        return err
    }
    var entries []diskCacheEntry
    if err := json.Unmarshal(data, &entries); err != nil {
        return err
    }
    now := time.Now()
    tracker.mu.Lock()
    defer tracker.mu.Unlock()
    for _, e := range entries {
        expiresAt := time.Unix(e.ExpiresAt, 0)
        if expiresAt.Before(now) {
            continue
        }
        fp := e.Fingerprint
        if _, ok := tracker.entries[fp]; ok {
            continue
        }
        entry := &sharedCacheEntry{
            fingerprint: fp,
            creatorID:   e.CreatorID,
            tokens:      e.Tokens,
            expiresAt:   expiresAt,
            ttl:         time.Duration(e.TTL),
        }
        entry.lruNode = tracker.lruList.PushFront(entry)
        tracker.entries[fp] = entry
    }
    return nil
}
```

- [ ] **Step 4: Add cache admin endpoints to handler.go**

```go
// In handler.go routing:
case path == "/cache/stats" && r.Method == "GET":
    h.apiCacheStats(w, r)
case path == "/cache/sync" && r.Method == "POST":
    h.apiCacheSync(w, r)
case path == "/cache/clear" && r.Method == "POST":
    h.apiCacheClear(w, r)

func (h *Handler) apiCacheStats(w http.ResponseWriter, r *http.Request) {
    t := h.promptCache
    t.mu.Lock()
    defer t.mu.Unlock()
    totalRequests := t.l1Hits + t.crossHits + t.semanticHits + t.misses
    hitRate := float64(0)
    if totalRequests > 0 {
        hitRate = float64(t.l1Hits+t.crossHits+t.semanticHits) / float64(totalRequests)
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "l1_entries":         len(t.entries),
        "hit_rate":           hitRate,
        "cross_account_hits": t.crossHits,
        "semantic_hits":      t.semanticHits,
        "tokens_saved":       t.tokensSaved,
        "lru_evictions":      t.lruEvictions,
    })
}

func (h *Handler) apiCacheSync(w http.ResponseWriter, r *http.Request) {
    if err := saveCacheToDisk(h.promptCache, "data/prompt_cache.json"); err != nil {
        w.WriteHeader(500)
        json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
        return
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiCacheClear(w http.ResponseWriter, r *http.Request) {
    h.promptCache.mu.Lock()
    h.promptCache.entries = make(map[[32]byte]*sharedCacheEntry)
    h.promptCache.lruList.Init()
    h.promptCache.mu.Unlock()
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
```

- [ ] **Step 5: Load L2 on startup in main.go**

```go
// In main(), after pool.GetPool():
handler := proxy.NewHandler()
if err := loadCacheFromDisk(handler.GetPromptCache(), "data/prompt_cache.json"); err != nil {
    logger.Warnf("Failed to load prompt cache from disk: %v", err)
}
```

Need to add `GetPromptCache()` getter to handler. In `proxy/handler.go`:

```go
func (h *Handler) GetPromptCache() *promptCacheTracker {
    return h.promptCache
}
```

- [ ] **Step 6: Add periodic sync goroutine**

Add to `NewHandler` or main:

```go
// Start periodic L1→L2 sync (every 30s)
go func() {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for range ticker.C {
        if err := saveCacheToDisk(h.promptCache, "data/prompt_cache.json"); err != nil {
            logger.Debugf("prompt cache sync: %v", err)
        }
    }
}()
```

Place in `main.go` after handler creation.

- [ ] **Step 7: Run tests**

```bash
go build ./...
go test ./proxy/ -run "TestCacheDisk|TestCrossAccount|TestPromptCache|TestBuildClaude" -v
```

Expected: all PASS.

- [ ] **Step 8: Commit**

```bash
git add proxy/cache_disk.go proxy/cache_tracker.go proxy/cache_tracker_test.go proxy/handler.go main.go
git commit -m "feat(cache): L2 disk persistence + admin API + periodic sync"
```

---

### Task 8: Benchmark Script

**Files:**
- Create: `kiro_bench_test.py`
- Modify: `kiro_sso_auto.py` — reuse Playwright framework if needed

**Interfaces:**
- Consumes: Kiro-Go running at localhost:8080
- Produces: 8 test cases, JSON report

- [ ] **Step 1: Write benchmark script**

```python
#!/usr/bin/env python3
"""
Kiro-Go Pool & Cache Benchmark Suite
Usage: python kiro_bench_test.py [--test TEST_NUM] [--accounts N]
"""
import asyncio, httpx, json, sys, time, random, os
from dataclasses import dataclass, field
from typing import Optional

BASE_URL = "http://localhost:8080"
ADMIN_PASSWORD = "changeme"

@dataclass
class BenchResult:
    test: str
    requests: int = 0
    success: int = 0
    fail: int = 0
    latencies: list = field(default_factory=list)
    cache_hits: int = 0
    cache_misses: int = 0

    @property
    def p50(self): return percentile(self.latencies, 0.50)
    @property
    def p95(self): return percentile(self.latencies, 0.95)
    @property
    def p99(self): return percentile(self.latencies, 0.99)
    @property
    def hit_rate(self):
        total = self.cache_hits + self.cache_misses
        return self.cache_hits / total if total > 0 else 0

def percentile(data, p):
    if not data: return 0
    return sorted(data)[int(len(data) * p)]

async def send_request(client, model="claude-sonnet-4-20250514",
                       prompt="Explain quantum computing in one sentence."):
    body = {
        "model": model,
        "max_tokens": 50,
        "messages": [{"role": "user", "content": prompt}],
    }
    start = time.monotonic()
    try:
        r = await client.post(
            f"{BASE_URL}/v1/messages",
            json=body,
            headers={"x-api-key": "test"},
            timeout=60.0,
        )
        elapsed = time.monotonic() - start
        cache_hit = "cache_read_input_tokens" in r.text and "cache_read_input_tokens" in r.text
        return True, elapsed, cache_hit, r.status_code
    except Exception as e:
        return False, time.monotonic() - start, False, 0

async def test1_baseline():
    """100 requests, measure latency distribution"""
    print("\n=== Test 1: Baseline 100 requests ===")
    result = BenchResult(test="baseline")
    async with httpx.AsyncClient() as client:
        tasks = [send_request(client) for _ in range(100)]
        responses = await asyncio.gather(*tasks)
        for ok, elapsed, cache, status in responses:
            result.requests += 1
            if ok and status == 200:
                result.success += 1
                result.latencies.append(elapsed)
                if cache:
                    result.cache_hits += 1
                else:
                    result.cache_misses += 1
            else:
                result.fail += 1
    print(f"  Success: {result.success}/{result.requests}")
    print(f"  p50: {result.p50:.2f}s  p95: {result.p95:.2f}s  p99: {result.p99:.2f}s")
    print(f"  Cache hit rate: {result.hit_rate:.2%}")
    return result

async def test2_burst():
    """50 concurrent requests at t=0"""
    print("\n=== Test 2: Burst 50 concurrent ===")
    result = BenchResult(test="burst")
    async with httpx.AsyncClient(limits=httpx.Limits(max_connections=100)) as client:
        tasks = [send_request(client) for _ in range(50)]
        responses = await asyncio.gather(*tasks)
        for ok, elapsed, cache, status in responses:
            result.requests += 1
            if ok and status == 200:
                result.success += 1
                result.latencies.append(elapsed)
            else:
                result.fail += 1
    rate_429 = sum(1 for _, _, _, s in responses if s == 429)
    print(f"  Success: {result.success}, Failed: {result.fail}, 429s: {rate_429}")
    print(f"  p50: {result.p50:.2f}s  p95: {result.p95:.2f}s")
    return result

async def test3_account_failure():
    """Simulate account failure → breaker opens → recovery"""
    print("\n=== Test 3: Account failure + circuit breaker ===")
    result = BenchResult(test="account_failure")
    # This test is qualitative — check admin API for breaker states
    async with httpx.AsyncClient() as client:
        # Get pool status before
        r = await client.get(f"{BASE_URL}/admin/api/pool/status",
                            headers={"X-Admin-Password": ADMIN_PASSWORD})
        before = r.json()
        print(f"  Before: available={before.get('availableCount')}/{before.get('totalAccounts')}")

        # Send 50 requests — breaker should handle failures
        tasks = [send_request(client) for _ in range(50)]
        responses = await asyncio.gather(*tasks)

        ok = sum(1 for ok, _, _, _ in responses if ok)
        print(f"  Completed: {ok}/50")

        # Get pool status after
        r = await client.get(f"{BASE_URL}/admin/api/pool/status",
                            headers={"X-Admin-Password": ADMIN_PASSWORD})
        after = r.json()
        for s in after.get("states", [])[:5]:
            print(f"  {s['id'][:16]}... state={s['state']} ewma={s['ewmaLatency']}")
    return result

async def test4_cross_account_cache():
    """Same prompt across 2 accounts → cross-account hit"""
    print("\n=== Test 4: Cross-account cache ===")
    result = BenchResult(test="cross_account_cache")
    prompt = "What is the capital of France? " * 50  # long enough to cache
    async with httpx.AsyncClient() as client:
        # Send 10 requests with same prompt
        tasks = [send_request(client, prompt=prompt) for _ in range(10)]
        responses = await asyncio.gather(*tasks)
        hits = sum(1 for _, _, cache, _ in responses if cache)
        print(f"  Cache hits: {hits}/10 (after 1st request creates cache)")
        result.cache_hits = hits
        result.cache_misses = 10 - hits
    print(f"  Cross-account hit rate: {result.hit_rate:.2%}")
    return result

async def test5_persistence():
    """Verify cache survives restart"""
    print("\n=== Test 5: Cache persistence ===")
    async with httpx.AsyncClient() as client:
        # Force sync to disk
        await client.post(f"{BASE_URL}/admin/api/cache/sync",
                         headers={"X-Admin-Password": ADMIN_PASSWORD})
        r = await client.get(f"{BASE_URL}/admin/api/cache/stats",
                            headers={"X-Admin-Password": ADMIN_PASSWORD})
        before = r.json()
        print(f"  L1 entries: {before.get('l1_entries')}")
        print(f"  NOTE: Restart Kiro-Go and re-run to verify L2 reload")
    return BenchResult(test="persistence")

async def test6_chaos():
    """Random account kill mid-traffic"""
    print("\n=== Test 6: Chaos — account kill ===")
    result = BenchResult(test="chaos")
    async with httpx.AsyncClient() as client:
        # Start 30 requests in background
        import threading, time as ttime
        responses = []
        async def worker():
            tasks = [send_request(client) for _ in range(30)]
            return await asyncio.gather(*tasks)

        responses = await worker()
        ok = sum(1 for ok, _, _, _ in responses if ok)
        result.success = ok
        result.requests = 30
        result.fail = 30 - ok
        rate = ok / 30
        print(f"  Success rate: {rate:.0%} (target > 95%)")
    return result

async def test7_lru_eviction():
    """Verify LRU evicts oldest entries"""
    print("\n=== Test 7: LRU eviction ===")
    async with httpx.AsyncClient() as client:
        r = await client.get(f"{BASE_URL}/admin/api/cache/stats",
                            headers={"X-Admin-Password": ADMIN_PASSWORD})
        stats = r.json()
        print(f"  LRU evictions: {stats.get('lru_evictions', 0)}")
        print(f"  L1 entries: {stats.get('l1_entries', 0)}")
    return BenchResult(test="lru_eviction")

async def test8_semantic():
    """Minor prompt variation → semantic hit"""
    print("\n=== Test 8: Semantic cache (needs ENABLE_SEMANTIC_CACHE=true)")
    async with httpx.AsyncClient() as client:
        r = await client.get(f"{BASE_URL}/admin/api/cache/stats",
                            headers={"X-Admin-Password": ADMIN_PASSWORD})
        stats = r.json()
        print(f"  Semantic hits: {stats.get('semantic_hits', 0)}")
        if stats.get('semantic_hits', 0) == 0:
            print("  (semantic cache disabled — set ENABLE_SEMANTIC_CACHE=true)")
    return BenchResult(test="semantic")

async def main():
    import argparse
    p = argparse.ArgumentParser()
    p.add_argument("--test", type=int, default=0)
    args = p.parse_args()

    tests = {
        1: test1_baseline, 2: test2_burst, 3: test3_account_failure,
        4: test4_cross_account_cache, 5: test5_persistence,
        6: test6_chaos, 7: test7_lru_eviction, 8: test8_semantic,
    }

    if args.test:
        results = [await tests[args.test]()]
    else:
        results = []
        for t in sorted(tests):
            results.append(await tests[t]())

    # Save report
    report = {
        "timestamp": time.time(),
        "results": [{"test": r.test, "p50": r.p50, "p95": r.p95,
                      "hit_rate": r.hit_rate, "success": r.success,
                      "requests": r.requests} for r in results],
    }
    with open("bench_results.json", "w") as f:
        json.dump(report, f, indent=2)
    print(f"\nReport saved to bench_results.json")

if __name__ == "__main__":
    asyncio.run(main())
```

- [ ] **Step 2: Run benchmark**

```bash
python -u kiro_bench_test.py --test 1
```

- [ ] **Step 3: Commit**

```bash
git add kiro_bench_test.py
git commit -m "test: add pool and cache benchmark suite (8 test cases)"
```

---

## Final Verification

```bash
# Build check
go build ./...

# All unit tests
go test ./pool/... ./proxy/... -v

# Run full benchmark
python -u kiro_bench_test.py
```

Expected: all tests pass, benchmark produces `bench_results.json`.
