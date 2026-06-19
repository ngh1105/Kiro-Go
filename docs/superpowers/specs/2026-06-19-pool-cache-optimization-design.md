# Kiro-Go Pool & Cache Optimization — Design Spec

Date: 2026-06-19 | Branch: `feat/azure-tenant-sso` | Status: Draft

## Overview

Optimize the two core subsystems of Kiro-Go's proxy layer: the account pool (load balancing / dispatch) and the prompt cache tracker (Claude prompt caching simulation). The goal is production-grade reliability, throughput, and cache efficiency.

---

## 1. Pool — Dynamic Dispatch Engine

### 1.1 Current State

**File:** `pool/account.go`

- Weighted round-robin via `atomic.AddUint64` over a weighted-duplicate slice
- Binary cooldown: 3 consecutive errors → 1 min, quota error → 1 hour
- Token expiry check (120s skew buffer)
- Quota blocking (`isQuotaBlocked`)
- Model-aware routing (`GetNextForModelExcluding`)
- Fallback: pick account with earliest cooldown expiry

**Problems:**
1. Weight=3 → 3 copies in slice. When one copy goes into cooldown the weight effectively drops to 2, and the remaining copies may still be tried (wasted iterations).
2. Cooldown is binary (on/off) — no gradual recovery. A transient network blip penalizes an account identically to a hard upstream outage.
3. No latency awareness. A slow account gets equal weight to a fast one.
4. No concurrency control. All goroutines hammer `GetNext` without backpressure.
5. `O(n)` traversal per `GetNext` call, with per-call `seen` map allocation.

### 1.2 New Design

#### Circuit Breaker (3-state, replaces binary cooldown)

Per-account state machine:

```
  CLOSED ──(N consecutive errors)──→ OPEN
    ↑                                    │
    │                              (timeout T)
    │                                    ↓
    └────────(1 success)─────── HALF_OPEN
                                    │
                            (1 failure) → OPEN
```

Config:
- `N` (error threshold) = 3
- `T` (open timeout) = 30s for transient errors, 300s for quota errors, 3600s for auth failures
- `DISABLED` terminal state: auth revocation, manual disable — requires operator re-enable

#### Dynamic Weight (EWMA Latency)

```go
type AccountState struct {
    config.Account
    breaker      CircuitBreaker
    ewmaLatency  float64  // nanoseconds, α = 0.2
    successCount uint64
    errorCount   uint64
    lastUsed     time.Time
}

// effectiveWeight = baseWeight * (targetLatency / ewmaLatency)
// Clamped to [1, baseWeight * 3]
```

- `targetLatency` = EWMA across all accounts (adaptive)
- α = 0.2: 5 requests to 67% weight of new observation
- Weight recalculated on each `RecordSuccess` (async, no lock contention on hot path)

#### Request Queue + Concurrency Limiter

```go
type AccountPool struct {
    states       []*AccountState
    currentIndex uint64
    sem          chan struct{}  // buffered channel = max concurrency
    queueTimeout time.Duration  // 30s
}
```

- `maxConcurrent = len(accounts) * 3`
- `sem <- struct{}{}` before dispatch, release on completion
- If sem full → wait with timeout → HTTP 429 on timeout
- Admin endpoint: `GET /admin/api/pool/status` → concurrent, queue depth, states

#### GetNext Algorithm (optimized)

```
1. Atomic increment currentIndex
2. Walk states starting from index (not weighted slice)
3. For each account:
   a. Circuit breaker OPEN? → skip
   b. Token expiring? → skip
   c. Quota blocked? → skip
   d. Model not supported? → skip
   e. Account busy (sem full for this account)? → skip
   f. Return account (CLOSED or HALF_OPEN)
4. Fallback: pick HALF_OPEN account with earliest OPEN→HALF_OPEN transition
5. No account: block on sem with timeout
```

**Eliminates weighted-duplicate slice.** Weight is applied via a selection probability step instead: after step 3 finds candidates, pick among them weighted by `effectiveWeight` using a cumulative distribution.

---

## 2. Cache — Cross-Account + Persistent + LRU

### 2.1 Current State

**File:** `proxy/cache_tracker.go`

- Per-account in-memory `map[[32]byte]promptCacheEntry`
- SHA-256 fingerprint of canonicalized prompt blocks
- TTL: 5 min (default) or 1 hour (cache_control with longer TTL)
- Min token threshold: 1024 (default), 4096 (Opus)
- 85% cap on cacheable portion per request
- First request → creation only, subsequent → match by fingerprint

**Problems:**
1. In-memory only — lost on restart.
2. Per-account — same prompt across accounts = no cache benefit. Each account rebuilds the same cache.
3. Exact match only — no partial/fuzzy matching for nearly-identical prompts.
4. No eviction besides TTL — memory grows unbounded.
5. Cache stats not exposed for observability.

### 2.2 New Design

#### Cross-Account Fingerprint Pool

```go
type sharedCacheEntry struct {
    fingerprint [32]byte
    creatorID   string    // account that first created this entry
    tokens      int
    expiresAt   time.Time
    ttl         time.Duration
    lruNode     *list.Element
}
```

- Single `map[[32]byte]*sharedCacheEntry` shared across all accounts
- When account B hits cache created by account A: billing credit attributed to A's `CacheReadInputTokens`
- `Compute()` checks shared pool first, then falls back to creation

#### 2-Tier Storage

| Tier | Storage | Capacity | Sync |
|------|---------|----------|------|
| L1 | In-memory `map` + LRU list | Unlimited (memory-bound) | — |
| L2 | `data/prompt_cache.json` | 10,000 entries | Batch write every 30s |

**L1 → L2 sync:**
```go
func (s *cacheStore) syncToDisk() {
    s.mu.RLock()
    // Collect non-expired entries
    entries := make([]cacheDiskEntry, 0, len(s.entries))
    for fp, e := range s.entries {
        if e.expiresAt.After(time.Now()) {
            entries = append(entries, cacheDiskEntry{...})
        }
    }
    s.mu.RUnlock()
    // Write atomically (write temp + rename)
    json.Marshal(entries) → data/prompt_cache.json.tmp → os.Rename
}
```

**Startup load:** `data/prompt_cache.json` → L1 on `main()` init. Skip expired entries.

#### LRU Eviction

```go
type cacheStore struct {
    mu       sync.RWMutex
    entries  map[[32]byte]*sharedCacheEntry
    lruList  *list.List
    maxSize  int  // 10,000
    l1Hits   uint64
    l2Hits   uint64
    misses   uint64
    crossHits uint64
    semanticHits uint64
    tokensSaved uint64
}
```

- On insert: if `len(entries) >= maxSize`, evict LRU tail
- On access: move entry to front of LRU list
- LRU respects expiry: expired entries evicted first, then LRU

#### Semantic Similarity Matching (feature flag)

```go
// ENABLE_SEMANTIC_CACHE=true (default: false)
```

When exact fingerprint match misses:
1. Compute MinHash signature of prompt (4-byte shingles, 128 hash functions)
2. Compare Jaccard similarity against L1 entries
3. If `similarity > 0.85` → treat as cache hit, flag `semantic: true`
4. Report `semantic_hits` in stats

Rationale: Claude Code often sends minor variations of the same system prompt (timestamp changes, project name changes). Semantic matching captures these near-duplicates.

#### Admin API

```
GET /admin/api/cache/stats
```
```json
{
  "l1_entries": 1234,
  "l2_entries": 8900,
  "hit_rate": 0.73,
  "cross_account_hits": 45,
  "semantic_hits": 3,
  "tokens_saved": 1234567,
  "lru_evictions": 12,
  "l2_syncs": 240
}
```

```
POST /admin/api/cache/clear   — flush all cache
POST /admin/api/cache/sync     — force L1→L2 sync
```

---

## 3. Test Plan

### 3.1 Benchmark Script

**File:** `kiro_bench_test.py`

```python
# Uses asyncio + httpx to send requests through Kiro-Go proxy
# Measures: latency (p50/p95/p99), success rate, cache hit rate
# Configurable: concurrency, request count, model, prompt templates
# Output: JSON report + console summary
```

### 3.2 Test Cases

| # | Test | What It Validates |
|---|------|-------------------|
| 1 | **Baseline** — 100 requests, 10 accounts, sonnet | p50/p95/p99 latency, success rate, cache hit rate. Compare before/after. |
| 2 | **Burst** — 50 concurrent at t=10s | Circuit breaker opens under load? Queue backpressure? 429 returned? Recovery? |
| 3 | **Account failure** — inject 500 errors on account #3 | CLOSED→OPEN after 3 errors. Requests skip #3. Stop errors. HALF_OPEN→CLOSED. |
| 4 | **Cross-account cache** — A sends prompt, B sends same | B gets cache hit from A's fingerprint. Hit rate > 0 before B creates own cache. |
| 5 | **Persistence** — 20 prompts, restart, 20 same prompts | All cache hits from L2 after restart. Hit rate = 1.0. |
| 6 | **Chaos** — random account kill mid-traffic | Pool rebalances. No dropped requests. Success rate > 95%. |
| 7 | **LRU eviction** — insert 11K entries (max=10K) | Oldest 1K evicted. L2 entries count stable at 10K. |
| 8 | **Semantic** — prompt with minor diff (timestamp change) | Similarity > 0.85 → semantic hit. Exact match miss but semantic match works. |

### 3.3 Success Criteria

| Metric | Before | After (Target) |
|--------|--------|----------------|
| p95 latency | — | < 5s (same upstream) |
| Cache hit rate (10 accounts, same prompt) | 0% (per-account) | > 60% (cross-account) |
| Cache hit rate after restart | 0% | 100% |
| Success rate under account failure | ~90% | > 95% |
| Queue timeout rate (burst) | N/A | < 10% |
| LRU eviction count (sustained load) | N/A | Stable at max size |

---

## 4. Implementation Order

1. **Pool circuit breaker** — isolate breaker logic, write unit tests
2. **Pool dynamic weight** — EWMA tracking + weighted selection
3. **Pool concurrency limiter** — semaphore + queue + 429 response
4. **Cache cross-account** — shared fingerprint map
5. **Cache 2-tier + persistence** — L1/L2 + JSON file sync
6. **Cache LRU + stats** — eviction + admin API
7. **Cache semantic** — MinHash + Jaccard (feature flag)
8. **Benchmark script + all 8 tests**

---

## 5. Files Changed

| File | Change |
|------|--------|
| `pool/account.go` | Circuit breaker, EWMA, semaphore, weighted selection |
| `pool/account_test.go` | Unit tests for all new pool logic |
| `pool/breaker.go` | New: circuit breaker state machine |
| `proxy/cache_tracker.go` | Cross-account, L1/L2, LRU, semantic, stats |
| `proxy/cache_tracker_test.go` | Unit tests for all new cache logic |
| `proxy/cache_disk.go` | New: L2 persistence layer |
| `proxy/handler.go` | Wire new pool + cache, add admin endpoints, semaphore acquire/release |
| `main.go` | Load L2 cache on startup |
| `kiro_bench_test.py` | New: benchmark + test script |
