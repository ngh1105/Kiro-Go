# API-Key Region Auto-Detect Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Auto-detect the correct AWS region for an api_key account at import time by probing a region list, instead of hardcoding `us-east-1`.

**Architecture:** Reuse the existing `GetUsageLimits` (`proxy/kiro_api.go:164`) as a read-only probe signal by calling it on a per-candidate throwaway copy of the account pinned via `ApiRegion`. A probe loop (`refreshApiKeyInfoWithRegionProbe`) walks `apiKeyRegionCandidates(hint)` until one returns 200; a 401/402/403 stops the loop (bad key/payment), a 404 means "wrong region, try next". Batch import probes once with the first key and applies the detected region to the whole batch; single import probes the one key async. The frontend region input becomes optional (empty = auto-detect).

**Tech Stack:** Go 1.x (stdlib `net/http`, `strings`, `os`), existing `kiroRestHttpStore` seam + `roundTripFunc` for tests, vanilla JS/HTML web UI with `t()` i18n (`web/locales/{en,zh}.json`).

## Global Constraints

- **Security (spec §Security):** no cleartext key/token in any new path. The probe reuses `GetUsageLimits` (existing bearer handling, existing masking); probe logs must use `accountEmailForLog(&acc)` / masked forms only, never the raw key. No new admin route (reuse `/auth/apikeys-batch`, `/auth/credentials`, `/admin/api/accounts`).
- **i18n parity (project rule):** every new UI string appears in BOTH `web/locales/en.json` AND `web/locales/zh.json`. Only ADD keys; never rename or delete.
- **Non-breaking JSON (spec §Non-breaking):** no `config` struct field changes. Region detection mutates only the already-stored `Account.Region`/`Account.ApiRegion` at import time. `KIRO_APIKEY_REGIONS` is a new optional env var.
- **Gitignore (project rule):** never commit anything under `data/`, `.playwright-mcp/`, `*.png`, `proxy/cache_concurrent_burst_test.go`, or `.superpowers/`. Commit only the files this plan names.
- **Branch:** `feat/apikey-region-autodetect` (off `main` @ `ed0875e`). Spec is committed at `6877948`.

---

## File Structure

- **`proxy/kiro_api.go`** — new probe primitives (pure + one helper that calls the store). Adds: `defaultApiKeyRegions`, `apiKeyRegionCandidates`, `apiKeyProbeFatal`, `refreshApiKeyInfoWithRegionProbe`, `refreshApiKeyAccountWithRegionDetection`. All reuse existing `GetUsageLimits`; no new HTTP function.
- **`proxy/apikey_batch.go`** — `ImportApiKeys` gains a synchronous probe-once-per-batch step before the create loop; adds small helper `firstImportableApiKeyKey`.
- **`proxy/handler.go`** — `apiImportCredentials` and `apiAddAccount` api_key branches call the shared async helper `refreshApiKeyAccountWithRegionDetection`.
- **`proxy/kiro_api_region_test.go`** (new) — unit tests for candidates, classifier, probe loop (stubbed `kiroRestHttpStore`).
- **`proxy/apikey_batch_test.go`** — one new test asserting batch region detection.
- **`web/app.js`** — region inputs become optional in `modalApiKey`, `modalApiKeyBatch`, `importApiKey`, `importApiKeysBatch`, `importApiKeysSubmit`.
- **`web/locales/en.json`, `web/locales/zh.json`** — add `apikey.regionAuto`, `apikey.regionHint` to both.

---

### Task 1: Region candidate list + fatal classifier (pure functions)

**Files:**
- Modify: `proxy/kiro_api.go` (insert after `shouldProbeFallbackRegions` ends at line 161, before `GetUsageLimits` at line 163)
- Test: `proxy/kiro_api_region_test.go` (new file)

**Interfaces:**
- Consumes: none new (uses stdlib `strings`, `os`; `config` already imported).
- Produces: `apiKeyRegionCandidates(hintRegion string) []string`, `apiKeyProbeFatal(err error) bool`, and the package var `defaultApiKeyRegions`. Task 2's probe loop depends on `apiKeyRegionCandidates`; Task 2's test depends on `apiKeyProbeFatal`.

- [ ] **Step 1: Write the failing tests**

Create `proxy/kiro_api_region_test.go`:

```go
package proxy

import (
	"errors"
	"testing"

	"kiro-go/config"
)

// TestApiKeyRegionCandidatesHintFirstThenDefaults checks the hint leads, then the
// built-in defaults follow (de-duplicated).
func TestApiKeyRegionCandidatesHintFirstThenDefaults(t *testing.T) {
	got := apiKeyRegionCandidates("eu-west-1")
	if len(got) < 2 || got[0] != "eu-west-1" {
		t.Fatalf("hint should be first: %v", got)
	}
	if got[1] != "us-east-1" {
		t.Fatalf("default us-east-1 should follow hint: %v", got)
	}
}

// TestApiKeyRegionCandidatesDedupHint checks a hint that is also a default is not
// repeated.
func TestApiKeyRegionCandidatesDedupHint(t *testing.T) {
	got := apiKeyRegionCandidates("us-east-1")
	if len(got) == 0 || got[0] != "us-east-1" {
		t.Fatalf("us-east-1 should lead: %v", got)
	}
	count := 0
	for _, r := range got {
		if r == "us-east-1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("us-east-1 should appear once: %v", got)
	}
}

// TestApiKeyRegionCandidatesEnvOverride checks KIRO_APIKEY_REGIONS replaces the
// built-in defaults while the hint is still tried first.
func TestApiKeyRegionCandidatesEnvOverride(t *testing.T) {
	t.Setenv("KIRO_APIKEY_REGIONS", "ap-south-1, eu-west-1 ,ap-south-1")
	got := apiKeyRegionCandidates("us-east-1")
	assertOrder(t, got, []string{"us-east-1", "ap-south-1", "eu-west-1"})
}

// TestApiKeyRegionCandidatesEmptyHint checks an empty hint yields the defaults only.
func TestApiKeyRegionCandidatesEmptyHint(t *testing.T) {
	got := apiKeyRegionCandidates("")
	if len(got) == 0 || got[0] != "us-east-1" {
		t.Fatalf("empty hint should default to us-east-1 first: %v", got)
	}
}

// TestApiKeyProbeFatal checks the narrow status classifier.
func TestApiKeyProbeFatal(t *testing.T) {
	if apiKeyProbeFatal(nil) {
		t.Fatal("nil error should not be fatal")
	}
	fatal := []string{"HTTP 401: unauthorized", "HTTP 402: payment required", "HTTP 403: forbidden"}
	for _, msg := range fatal {
		if !apiKeyProbeFatal(errors.New(msg)) {
			t.Fatalf("apiKeyProbeFatal(%q) = false, want true", msg)
		}
	}
	nonfatal := []string{"HTTP 404: not found", "HTTP 500: internal", "Get q.foo.amazonaws.com: connection refused", ""}
	for _, msg := range nonfatal {
		if apiKeyProbeFatal(errors.New(msg)) {
			t.Fatalf("apiKeyProbeFatal(%q) = true, want false", msg)
		}
	}
}

// reference so the compiler sees config is used if candidates tests grow; keeps
// the import honest without a blank identifier.
var _ = config.Account{}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./proxy/ -run 'TestApiKeyRegionCandidates|TestApiKeyProbeFatal' -v`
Expected: FAIL — `undefined: apiKeyRegionCandidates` / `undefined: apiKeyProbeFatal`.

- [ ] **Step 3: Write minimal implementation**

Insert into `proxy/kiro_api.go` between the end of `shouldProbeFallbackRegions` (line 161) and `GetUsageLimits` (line 163):

```go
// defaultApiKeyRegions is the ordered set probed when an api_key's region is
// unknown. The hint region supplied by the caller is always tried first (see
// apiKeyRegionCandidates); this list follows. Override or replace with the
// KIRO_APIKEY_REGIONS env var (comma-separated).
var defaultApiKeyRegions = []string{
	"us-east-1", "us-east-2", "us-west-2",
	"eu-central-1", "eu-west-1", "eu-west-2",
	"ap-south-1", "ap-southeast-1", "ap-southeast-2",
	"ap-northeast-1", "ap-northeast-2",
	"ca-central-1", "il-central-1",
}

// apiKeyRegionCandidates returns the ordered, de-duplicated region list to probe
// for an api_key import. hintRegion is tried first (the user's guess, or the
// prior default). KIRO_APIKEY_REGIONS, when set, replaces the built-in list;
// otherwise defaultApiKeyRegions is used. Mirrors kiroProfileRegionCandidates'
// dedup style.
func apiKeyRegionCandidates(hintRegion string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(region string) {
		region = strings.TrimSpace(region)
		if region == "" || seen[region] {
			return
		}
		seen[region] = true
		out = append(out, region)
	}
	add(hintRegion)
	if env := strings.TrimSpace(os.Getenv("KIRO_APIKEY_REGIONS")); env != "" {
		for _, r := range strings.Split(env, ",") {
			add(r)
		}
		return out
	}
	for _, r := range defaultApiKeyRegions {
		add(r)
	}
	return out
}

// apiKeyProbeFatal reports whether a GetUsageLimits probe error means the key
// itself is unusable (auth failure or payment), so the region loop must STOP
// rather than try another region. Distinguished by the HTTP status embedded in
// GetUsageLimits's error ("HTTP %d: ..."): 401/402/403 → fatal; 404 and
// everything else → wrong region or transient, continue. Do NOT reuse
// isAuthErrorMessage — its word-list is broader and wrong for this narrow status.
func apiKeyProbeFatal(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "HTTP 401") ||
		strings.Contains(msg, "HTTP 402") ||
		strings.Contains(msg, "HTTP 403")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./proxy/ -run 'TestApiKeyRegionCandidates|TestApiKeyProbeFatal' -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add proxy/kiro_api.go proxy/kiro_api_region_test.go
git commit -m "feat(proxy): api_key region candidate list + probe classifier

Pure helpers for api_key region auto-detect: defaultApiKeyRegions (13 AWS Q
regions), apiKeyRegionCandidates (hint-first, KIRO_APIKEY_REGIONS override),
apiKeyProbeFatal (401/402/403 fatal, 404+ continue). No store access yet."
```

---

### Task 2: Probe loop (reuses GetUsageLimits) + single-import helper

**Files:**
- Modify: `proxy/kiro_api.go` (add two functions after `apiKeyProbeFatal`)
- Test: `proxy/kiro_api_region_test.go` (extend)

**Interfaces:**
- Consumes: `apiKeyRegionCandidates`, `apiKeyProbeFatal` (Task 1); existing `GetUsageLimits(*config.Account) (*UsageLimitsResponse, error)`, `accountEmailForLog`, `config.UpdateAccount`, `RefreshAccountInfo`, `logger`.
- Produces: `refreshApiKeyInfoWithRegionProbe(account *config.Account, hintRegion string) (*UsageLimitsResponse, string, error)` — used by Task 3 (batch) and Task 4 (single). `refreshApiKeyAccountWithRegionDetection(account *config.Account) (*config.AccountInfo, bool, error)` — used by Task 4.

**Design note:** The probe is unit-tested by stubbing `kiroRestHttpStore` (the existing seam) with a `roundTripFunc` that inspects `req.URL.Host`. `regionalizeURLForRegion` yields a distinct real host per region — `codewhisperer.us-east-1.amazonaws.com` for `us-east-1`, `q.<region>.amazonaws.com` otherwise — so a stub can return 200 for the target region and 404 for the rest, exercising the real region-resolution + host-rewrite path. No function-injection seam.

- [ ] **Step 1: Write the failing tests**

Append to `proxy/kiro_api_region_test.go` (add `io`, `net/http`, `strings` to the import block — replace the existing import block with):

```go
import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"kiro-go/config"
)
```

Then append the probe-loop tests:

```go
// TestRefreshApiKeyInfoWithRegionProbeHit walks candidates until the eu-central-1
// host returns 200, and asserts the input account is NOT mutated by the probe.
func TestRefreshApiKeyInfoWithRegionProbeHit(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			code, body := http.StatusNotFound, "not found"
			if req.URL.Host == "q.eu-central-1.amazonaws.com" {
				code, body = 200, `{}`
			}
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	acct := &config.Account{AuthMethod: "api_key", KiroApiKey: "KEY-XYZ", AccessToken: "KEY-XYZ", Region: "us-east-1"}
	usage, region, err := refreshApiKeyInfoWithRegionProbe(acct, "us-east-1")
	if err != nil {
		t.Fatalf("expected hit, got err %v", err)
	}
	if region != "eu-central-1" {
		t.Fatalf("region: want eu-central-1, got %q", region)
	}
	if usage == nil {
		t.Fatal("expected usage response, got nil")
	}
	if acct.Region != "us-east-1" || acct.ApiRegion != "" {
		t.Fatalf("probe mutated input account: Region=%q ApiRegion=%q", acct.Region, acct.ApiRegion)
	}
}

// TestRefreshApiKeyInfoWithRegionProbeStopsOnAuthError checks a 401 on the first
// candidate stops the loop (no further regions probed).
func TestRefreshApiKeyInfoWithRegionProbeStopsOnAuthError(t *testing.T) {
	var hits int
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hits++
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("unauthorized")), Header: make(http.Header)}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	acct := &config.Account{AuthMethod: "api_key", KiroApiKey: "BAD", AccessToken: "BAD", Region: "us-east-1"}
	_, _, err := refreshApiKeyInfoWithRegionProbe(acct, "us-east-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !apiKeyProbeFatal(err) {
		t.Fatalf("expected fatal error, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 probe hit (stop on 401), got %d", hits)
	}
}

// TestRefreshApiKeyInfoWithRegionProbeAllMiss checks an all-404 sweep returns the
// last error and an empty detected region.
func TestRefreshApiKeyInfoWithRegionProbeAllMiss(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("nf")), Header: make(http.Header)}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	acct := &config.Account{AuthMethod: "api_key", KiroApiKey: "KEY", AccessToken: "KEY", Region: "us-east-1"}
	_, region, err := refreshApiKeyInfoWithRegionProbe(acct, "")
	if err == nil {
		t.Fatal("expected error after all-404, got nil")
	}
	if region != "" {
		t.Fatalf("region: want empty on miss, got %q", region)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./proxy/ -run 'TestRefreshApiKeyInfoWithRegionProbe' -v`
Expected: FAIL — `undefined: refreshApiKeyInfoWithRegionProbe`.

- [ ] **Step 3: Write minimal implementation**

Append to `proxy/kiro_api.go` after `apiKeyProbeFatal`:

```go
// refreshApiKeyInfoWithRegionProbe probes apiKeyRegionCandidates(hintRegion) in
// order, returning the usage response + the region that worked. For each
// candidate it builds a throwaway shallow copy of account pinned to that region
// (copy.ApiRegion = candidate, the highest-priority key in EffectiveApiRegion)
// and calls the existing GetUsageLimits(&copy). Stop conditions:
//   - 200              → HIT (return usage, candidate, nil)
//   - apiKeyProbeFatal → key bad / payment → STOP (return nil, "", err)
//   - 404 / 5xx / net  → wrong region or transient → next candidate
// Returns the last error when the list is exhausted (caller falls back to the
// hint/default region). Read-only: it does NOT persist and does NOT mutate the
// input account (only the per-region local copy).
func refreshApiKeyInfoWithRegionProbe(account *config.Account, hintRegion string) (*UsageLimitsResponse, string, error) {
	var lastErr error
	for _, region := range apiKeyRegionCandidates(hintRegion) {
		probe := *account // shallow copy; GetUsageLimits is read-only for api_key
		probe.ApiRegion = region
		probe.Region = region
		usage, err := GetUsageLimits(&probe)
		if err == nil {
			return usage, region, nil
		}
		if apiKeyProbeFatal(err) {
			return nil, "", err
		}
		lastErr = err
	}
	return nil, "", lastErr
}

// refreshApiKeyAccountWithRegionDetection is the single-import helper: probe the
// api_key's region; if a region different from the account's is detected, persist
// it and return regionChanged=true so the caller can reload its pool. Then run the
// best-effort RefreshAccountInfo. Best-effort + async-safe: a probe failure leaves
// the account with its original region. The caller MUST run this in a goroutine
// (it makes upstream calls).
func refreshApiKeyAccountWithRegionDetection(account *config.Account) (info *config.AccountInfo, regionChanged bool, err error) {
	if account != nil {
		if _, detected, probeErr := refreshApiKeyInfoWithRegionProbe(account, account.Region); probeErr == nil && detected != "" && detected != account.Region {
			account.Region = detected
			account.ApiRegion = detected
			if updateErr := config.UpdateAccount(account.ID, *account); updateErr != nil {
				logger.Warnf("[ApiKeyRegion] failed to persist detected region %s for %s: %v", detected, accountEmailForLog(account), updateErr)
			} else {
				regionChanged = true
			}
		}
	}
	info, err = RefreshAccountInfo(account)
	return info, regionChanged, err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./proxy/ -run 'TestRefreshApiKeyInfoWithRegionProbe|TestApiKeyRegionCandidates|TestApiKeyProbeFatal' -v`
Expected: PASS (all 8 tests).

- [ ] **Step 5: Commit**

```bash
git add proxy/kiro_api.go proxy/kiro_api_region_test.go
git commit -m "feat(proxy): api_key region probe loop

refreshApiKeyInfoWithRegionProbe walks apiKeyRegionCandidates, calling the
existing GetUsageLimits on a per-region throwaway copy. 200=hit, 401/402/403=stop,
404=next. Read-only, no input mutation. refreshApiKeyAccountWithRegionDetection
wraps it for single-import: detect+persist region, then RefreshAccountInfo."
```

---

### Task 3: Batch import probe-once-per-batch

**Files:**
- Modify: `proxy/apikey_batch.go` (`ImportApiKeys` @ line 50; add helper near top)
- Test: `proxy/apikey_batch_test.go` (extend)

**Interfaces:**
- Consumes: `refreshApiKeyInfoWithRegionProbe` (Task 2); existing `config.AddAccount`, `RefreshAccountInfo`, `h.pool.Reload`, `maskKey`, `config.GenerateMachineId`.
- Produces: behavior change in `ImportApiKeys` — persisted accounts now carry the detected region.

- [ ] **Step 1: Write the failing test**

Append to `proxy/apikey_batch_test.go`:

```go
// TestImportApiKeysDetectsBatchRegion verifies the first-key probe detects the
// batch region and applies it to every persisted account. Only the eu-central-1
// host returns 200; us-east-1 (the hint) 404s, so detection must fall back to
// eu-central-1.
func TestImportApiKeysDetectsBatchRegion(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			code, body := http.StatusNotFound, "not found"
			if req.URL.Host == "q.eu-central-1.amazonaws.com" {
				code, body = 200, `{}`
			}
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	h := &Handler{pool: accountpool.GetPool()}
	summary := h.ImportApiKeys("KEY-A-1234567890\nKEY-B-1234567890", "us-east-1", "", "")
	if summary.Imported != 2 {
		t.Fatalf("imported: want 2, got %d", summary.Imported)
	}
	for _, a := range config.GetAccounts() {
		if a.Region != "eu-central-1" {
			t.Fatalf("account %s Region: want eu-central-1, got %q", a.ID, a.Region)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./proxy/ -run TestImportApiKeysDetectsBatchRegion -v`
Expected: FAIL — `Region: want eu-central-1, got "us-east-1"` (current code keeps the hint).

- [ ] **Step 3: Write minimal implementation**

Add a helper near the top of `proxy/apikey_batch.go` (after the imports block, before `ApiKeyImportResult` at line 13):

```go
// firstImportableApiKeyKey returns the first non-empty, non-duplicate key in
// rawText, used as the batch region probe. Duplicates already present in existing
// and repeats within rawText are skipped. Returns "" when there is no importable
// key. This is a focused early-out scan; it does not replicate the create loop's
// per-key create/refresh/async work.
func firstImportableApiKeyKey(rawText string, existing map[string]bool) string {
	seenInBatch := make(map[string]bool)
	for _, raw := range strings.Split(rawText, "\n") {
		key := strings.TrimSpace(raw)
		if key == "" || existing[key] || seenInBatch[key] {
			continue
		}
		seenInBatch[key] = true
		return key
	}
	return ""
}
```

In `ImportApiKeys`, insert the probe step AFTER the `if region == "" { region = "us-east-1" }` block (line 64) and BEFORE the `for _, raw := range strings.Split(rawText, "\n") {` loop (line 66). The inserted block:

```go
	// Probe-once-per-batch: detect the batch region from the first importable key.
	// Read-only; the probe account is thrown away (no persist, no double-create).
	// On HIT override region with the detected one; on failure keep the hint/default.
	if first := firstImportableApiKeyKey(rawText, existing); first != "" {
		probeAccount := config.Account{
			AuthMethod:  "api_key",
			KiroApiKey:  first,
			AccessToken: first,
			MachineId:   config.GenerateMachineId(),
		}
		if _, detected, err := refreshApiKeyInfoWithRegionProbe(&probeAccount, region); err == nil && detected != "" {
			region = detected
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./proxy/ -run 'TestImportApiKeys' -v`
Expected: PASS — `TestImportApiKeysDetectsBatchRegion`, `TestImportApiKeysDedupAndPerKeyResult`, `TestApiImportApiKeysHandler`, `TestApiImportApiKeysRejectsEmpty` all pass. (Existing tests stub uniform 404 or 200; with uniform 404 the probe fully misses and region stays the hint, so `TestImportApiKeysDedupAndPerKeyResult` is unaffected. With uniform 200 the probe hits the first candidate — `us-east-1`/hint — and keeps it; `TestApiImportApiKeysHandler` passes through.)

- [ ] **Step 5: Commit**

```bash
git add proxy/apikey_batch.go proxy/apikey_batch_test.go
git commit -m "feat(proxy): probe region once per api_key batch import

ImportApiKeys detects the batch region from the first importable key before
creating accounts, then applies it to every key. Same-region batch = one probe.
Mixed-region keys in other regions still 404 on their own refresh (InfoFailed,
created anyway). Bad key stops the probe early (401/402/403)."
```

---

### Task 4: Single-import region detection (apiImportCredentials + apiAddAccount)

**Files:**
- Modify: `proxy/handler.go` (`apiImportCredentials` api_key branch @ 3522-3526; `apiAddAccount` @ 2671-2679)
- Test: existing `proxy/import_credentials_test.go` tests must still pass (no new test — wiring verified by build + smoke per spec §Testing).

**Interfaces:**
- Consumes: `refreshApiKeyAccountWithRegionDetection` (Task 2); existing `logger`, `accountEmailForLog`, `h.pool.Reload`.
- Produces: detected region persisted on single api_key imports.

**Design note:** The probe runs INSIDE the existing async goroutine so import response latency is unaffected. The account is persisted immediately with the user's region (or `us-east-1` default); the goroutine detects, persists the new region, reloads the pool, then refreshes usage info. A chat request arriving in the probe window uses the old region — acceptable (best-effort, matches the existing async-`RefreshAccountInfo` behavior).

- [ ] **Step 1: Wire `apiImportCredentials`**

In `proxy/handler.go`, replace the existing api_key async refresh goroutine (lines 3522-3526):

```go
		go func(acc config.Account) {
			if _, infoErr := RefreshAccountInfo(&acc); infoErr != nil {
				logger.Warnf("[Import] RefreshAccountInfo failed for api_key account %s: %v", accountEmailForLog(&acc), infoErr)
			}
		}(account)
```

with:

```go
		go func(acc config.Account) {
			_, regionChanged, infoErr := refreshApiKeyAccountWithRegionDetection(&acc)
			if regionChanged {
				h.pool.Reload()
			}
			if infoErr != nil {
				logger.Warnf("[Import] RefreshAccountInfo failed for api_key account %s: %v", accountEmailForLog(&acc), infoErr)
			}
		}(account)
```

- [ ] **Step 2: Wire `apiAddAccount`**

In `proxy/handler.go`, after `h.pool.Reload()` (line 2671) in `apiAddAccount`, insert an api_key probe+refresh goroutine BEFORE the existing `if account.Enabled && (...)` model-cache block (line 2673):

```go
	// api_key: best-effort region detection + usage refresh (async). The probe
	// persists a detected region and reloads the pool; a failure leaves the
	// account with the user's region (or us-east-1 default).
	if account.IsApiKeyCredential() {
		go func(acc config.Account) {
			_, regionChanged, infoErr := refreshApiKeyAccountWithRegionDetection(&acc)
			if regionChanged {
				h.pool.Reload()
			}
			if infoErr != nil {
				logger.Warnf("[AddAccount] RefreshAccountInfo failed for api_key account %s: %v", accountEmailForLog(&acc), infoErr)
			}
		}(account)
	}
```

- [ ] **Step 3: Run the affected tests to verify they still pass**

Run: `go test ./proxy/ -run 'TestApiImportCredentialsApiKey|TestApiAddAccount' -v`
Expected: PASS. `TestApiImportCredentialsApiKeyBranch` stubs uniform 200; the probe hits the first candidate (hint `eu-central-1`), detected == region → no change, then `RefreshAccountInfo` runs against the stub. Region assertion (`eu-central-1`) holds. `TestApiImportCredentialsApiKeyRequiresKey` 400s before any goroutine.

- [ ] **Step 4: Commit**

```bash
git add proxy/handler.go
git commit -m "feat(proxy): single api_key import region detection

apiImportCredentials + apiAddAccount api_key branches call
refreshApiKeyAccountWithRegionDetection inside the existing async goroutine:
detect region, persist + reload pool if changed, then refresh usage. Import
response latency unaffected."
```

---

### Task 5: Frontend optional region + i18n

**Files:**
- Modify: `web/app.js` (lines 2757, 2772, 2974, 2986, 2350)
- Modify: `web/locales/en.json` (after line 254), `web/locales/zh.json` (after line 254)

**Interfaces:**
- Consumes: existing `t()`, `escapeAttr`, `escapeHtml`, `$`, `api()`.
- Produces: region inputs send empty string when blank (backend auto-detects); two new i18n keys.

- [ ] **Step 1: Add i18n keys (both locales)**

In `web/locales/en.json`, after the line `"apikey.importSuccess": "Account added",` (line 254), add:

```json
  "apikey.regionAuto": "Auto-detect (leave empty)",
  "apikey.regionHint": "Leave empty to auto-detect the region by probing. Type a region to try it first.",
```

In `web/locales/zh.json`, after the line `"apikey.importSuccess": "账号添加成功",` (line 254), add:

```json
  "apikey.regionAuto": "自动识别（留空）",
  "apikey.regionHint": "留空将通过探测自动识别区域；输入区域将优先尝试该区域。",
```

- [ ] **Step 2: Make the single-add modal region input optional**

In `web/app.js` line 2757, replace:

```js
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="apiKeyRegion" value="us-east-1" /></div>' +
```

with:

```js
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="apiKeyRegion" placeholder="' + escapeAttr(t('apikey.regionAuto')) + '" /><small class="muted-text">' + escapeHtml(t('apikey.regionHint')) + '</small></div>' +
```

- [ ] **Step 3: Make the batch-add modal region input optional**

In `web/app.js` line 2772, replace:

```js
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="apiKeyBatchRegion" value="us-east-1" /></div>' +
```

with:

```js
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="apiKeyBatchRegion" placeholder="' + escapeAttr(t('apikey.regionAuto')) + '" /><small class="muted-text">' + escapeHtml(t('apikey.regionHint')) + '</small></div>' +
```

- [ ] **Step 4: Send empty region when blank (single + batch)**

In `web/app.js` line 2974, replace:

```js
    const region = $('apiKeyRegion').value || 'us-east-1';
```

with:

```js
    const region = $('apiKeyRegion').value.trim();
```

In `web/app.js` line 2986, replace:

```js
    const region = $('apiKeyBatchRegion').value || 'us-east-1';
```

with:

```js
    const region = $('apiKeyBatchRegion').value.trim();
```

- [ ] **Step 5: Drop the hardcoded region in the admin B4 import**

In `web/app.js` line 2350, replace:

```js
      var res = await api('/auth/apikeys-batch', { method: 'POST', body: JSON.stringify({ keys: keys, region: 'us-east-1' }) });
```

with:

```js
      var res = await api('/auth/apikeys-batch', { method: 'POST', body: JSON.stringify({ keys: keys }) });
```

- [ ] **Step 6: Verify i18n parity**

Run (PowerShell):
```powershell
$en = (Get-Content web/locales/en.json -Raw | ConvertFrom-Json).PSObject.Properties.Name
$zh = (Get-Content web/locales/zh.json -Raw | ConvertFrom-Json).PSObject.Properties.Name
"en keys: $($en.Count)  zh keys: $($zh.Count)"
"missing in zh: " + (($en | Where-Object { $_ -notin $zh }) -join ', ')
"missing in en: " + (($zh | Where-Object { $_ -notin $en }) -join ', ')
```
Expected: both counts equal; both "missing" lines empty.

- [ ] **Step 7: Commit**

```bash
git add web/app.js web/locales/en.json web/locales/zh.json
git commit -m "feat(web): api_key region input optional (auto-detect)

Single + batch add-account modals drop the us-east-1 default value, add an
auto-detect placeholder + hint, and send an empty region when blank (backend
probes). Admin B4 import omits region entirely. i18n: apikey.regionAuto +
apikey.regionHint added to en + zh."
```

---

### Task 6: Build + smoke verify

**Files:** none (verification only).

- [ ] **Step 1: Full test suite**

Run: `go test ./...`
Expected: PASS for `config`, `proxy` new+existing tests. Pre-existing flaky tests (`TestBuildKiroTransportFallsBackToEnvironmentProxy`, Windows TempDir cleanup races) may fail unrelated to this change — confirm they fail on `main` too before treating as a regression.

- [ ] **Step 2: Build the binary**

Run: `go build -o kiro-go.exe .`
Expected: `BUILD_OK`, no errors. Confirm embedded `vcs.revision` matches HEAD:
```powershell
go version -m kiro-go.exe | Select-String 'vcs.revision'
git rev-parse --short=12 HEAD
```
Expected: both SHA prefixes match; `vcs.modified=true` is fine (untracked junk only — `.playwright-mcp/`, `kiro-admin-login.png`, `proxy/cache_concurrent_burst_test.go`, `kiro-go.exe` itself).

- [ ] **Step 3: Smoke (manual, real upstream)**

Start the rebuilt binary against a real config with one api_key whose region is NOT us-east-1 (e.g. eu-central-1):
1. Import the key via the add-account modal with the region field LEFT EMPTY.
2. Observe logs: the probe walks candidates; a `[ApiKeyRegion]` persist line (or the account's Region field in the admin UI) shows the detected region.
3. Confirm the account gets email/usage (the per-key `RefreshAccountInfo` now hits the correct regional host).
4. Send one chat request through the account; confirm it routes to the detected region's host (no 404/403 region mismatch).

- [ ] **Step 4: Final state check**

Run: `git log --oneline main..HEAD`
Expected: 6 feature commits on top of `ed0875e` (spec commits `a1e9c88`, `7ed24df`, `6877948` are already on the branch).

No commit for this task (verification only).
