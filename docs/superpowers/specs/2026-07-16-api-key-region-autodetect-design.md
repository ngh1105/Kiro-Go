# API-Key Region Auto-Detect — Design

**Date:** 2026-07-16
**Status:** Approved (brainstormed 2026-07-16) → spec for implementation plan
**Branch base:** `main` @ `ed0875e`

## Problem

Importing a Kiro **api_key**-type account (`AuthMethod: "api_key"`) defaults the region to
`us-east-1` and never probes alternatives. If the key actually belongs to another region
(e.g. `eu-central-1`), the import succeeds (the account is created) but the upstream info
fetch (`GetUsageLimits`) returns **HTTP 404** and the account is left without email/usage
metadata. Worse, chat requests through that account are sent to the wrong region's host
and fail. The user must already know the correct region and type it — there is no
auto-detection and no fallback.

The machinery to detect region already exists for `external_idp`/`idc` accounts
(`kiroProfileRegionCandidates` + `resolveProfileArnAcrossRegions` at
`proxy/kiro_api.go:115/416`), but `api_key` accounts are explicitly excluded from it
(`shouldProbeFallbackRegions` at `proxy/kiro_api.go:152` returns false for `api_key`; and
`ensureRestProfileArn` short-circuits at `proxy/kiro_api.go:273`).

## Goal

When importing an api_key (batch or single), **auto-detect the correct region by probing**,
falling back through a region list until one returns 200. The user may leave the region
field empty ("auto-detect") or supply a hint that is tried first.

## Key constraint / signal (verified)

- `GetUsageLimits` (`proxy/kiro_api.go:164`) returns errors of the form `HTTP %d: <body>`
  (`kiro_api.go:186`). For an api_key with the **wrong** region it returns **HTTP 404**; for a
  genuinely invalid/forbidden key it returns **401/403**. The two are distinguishable.
- The auth-error classifier `isAuthErrorMessage` (`proxy/account_failover.go:79`) matches
  `http 401`/`http 403` (+ token/unauthorized words) but **not** `http 404`. So probing on a
  404 is safe — it never trips a "key is bad" classification, and `RefreshAccountInfo`
  already excludes api_key from the auth-error ban path (`proxy/kiro_api.go:568`).
- The region→host rewrite seam is `regionalizeURLForRegion(rawURL, region)`
  (`proxy/kiro_api.go:86`): rewrites `q.us-east-1.amazonaws.com` /
  `codewhisperer.us-east-1.amazonaws.com` → `q.<region>.amazonaws.com`.
- `GetUsageLimits` currently calls `regionalizeURL(url, account)` (`kiro_api.go:169`), which
  derives the region from the account. A probe needs to hit an **explicit** region without
  mutating shared account state.

## Design

### 1. Region-explicit probe — `proxy/kiro_api.go` (reuses `GetUsageLimits`, no duplication)

Reuse the existing `GetUsageLimits` (`kiro_api.go:164`) as the probe signal by calling it with
a **throwaway shallow copy** of the account whose region is pinned to the candidate. Region
resolution for api_key already flows through `account.EffectiveApiRegion()` (`config/config.go:262`,
chain `ApiRegion > Region > global > us-east-1`) inside `regionalizeURLForProfile` →
`kiroRegionForProfile` (`kiro_api.go:39`), so setting the copy's `ApiRegion` (the highest-priority
key in the chain) to the candidate guarantees the probe targets that region. `GetUsageLimits`
itself is unchanged, and `ensureRestProfileArn` still short-circuits for api_key
(`kiro_api.go:273`) so no profile-ARN resolution runs. No new GET function is added.

This is a read-only upstream GET — safe to call repeatedly during import without persisting anything.

### 2. Region candidate list (new) — `proxy/kiro_api.go`

Add:
```go
var defaultApiKeyRegions = []string{
    "us-east-1", "us-east-2", "us-west-2",
    "eu-central-1", "eu-west-1", "eu-west-2",
    "ap-south-1", "ap-southeast-1", "ap-southeast-2",
    "ap-northeast-1", "ap-northeast-2",
    "ca-central-1", "il-central-1",
}

func apiKeyRegionCandidates(hintRegion string) []string
```
`apiKeyRegionCandidates` mirrors the dedup style of `kiroProfileRegionCandidates`
(`kiro_api.go:115`): start with `hintRegion` (if non-empty), then either the `KIRO_APIKEY_REGIONS`
env override (comma-separated, when set) or `defaultApiKeyRegions`, dedup preserving order.

### 3. The probe loop (new) — `proxy/kiro_api.go`

```go
// apiKeyProbeFatal reports whether a probe error means the key itself is unusable
// (auth failure or payment), so the loop must STOP rather than try another region.
// Distinguish by the HTTP status embedded in GetUsageLimits's error ("HTTP %d: ..."):
// 401/403/402 → fatal (key/payment); 404 and everything else → wrong region, continue.
// Do NOT reuse isAuthErrorMessage — its word-list is broader and wrong for this
// narrow status signal.
func apiKeyProbeFatal(err error) bool

// refreshApiKeyInfoWithRegionProbe probes apiKeyRegionCandidates(hintRegion) in order.
// For each candidate it builds a throwaway shallow copy of account pinned to that
// region (copy.ApiRegion = candidate, so EffectiveApiRegion resolves to it) and calls
// the existing GetUsageLimits(&copy). Stop conditions:
//   - 200               → HIT (return usage, candidate, nil)
//   - apiKeyProbeFatal  → key bad / payment → STOP (return nil, "", err)
//   - 404 / 5xx / net   → wrong region or transient → next candidate
// Returns the last error when the list is exhausted (caller falls back to hint/default).
// Does not mutate the input account.
func refreshApiKeyInfoWithRegionProbe(account *config.Account, hintRegion string) (*UsageLimitsResponse, string, error)
```

No injected seam: the loop calls the real `GetUsageLimits` on each per-region copy. It is
unit-tested by stubbing `kiroRestHttpStore` with a transport that inspects `req.URL.Host`
(see Testing) — `regionalizeURLForRegion` yields a distinct real host per region, so the
stub can return 200 for the correct region and 404 for the others, exercising the real path.

### 4. Batch import — `proxy/apikey_batch.go` (`ImportApiKeys` @ :50)

"Probe once per batch" (chosen strategy):

1. Parse + dedup keys as today. Build `importable` (non-duplicate keys).
2. **Before** creating accounts: if there is at least one importable key, resolve the batch
   region with a single probe sweep. Build a throwaway probe account from `importable[0]`
   (`AuthMethod:"api_key"`, `KiroApiKey`=key, `AccessToken`=key) and call
   `refreshApiKeyInfoWithRegionProbe(&probeAccount, region)` once. On HIT (`detected != ""`)
   set `region = detected`; on probe failure keep the hint region (or `us-east-1`, defaulted
   just above). The probe is read-only and throws away its account — no persistence, no
   double-create. `importable[0]` is then created alongside the rest with the resolved region.
3. Then create **all** accounts (including the first key) with the now-resolved `region`,
   exactly as today: `account.Region = region`, `RefreshAccountInfo(&account)` per key
   (single call, **no re-probe**), persist, async model-cache refresh.

Result: a same-region batch is detected with one probe and every key gets correct usage info.
A key in a *different* region than the first still 404s on its own `RefreshAccountInfo` →
`InfoFailed++` (as today) and is created anyway — no regression.

Keep the existing `if region == "" { region = "us-east-1" }` default *after* the probe, so a
fully-failed probe (key bad, or every region 404) still produces a valid account with the
safe default region.

### 5. Single import — `proxy/handler.go`

Two single-import paths carry api_key and default `region = "us-east-1"`:

- `apiAddAccount` api_key branch (`handler.go:2643`).
- `apiImportCredentials` api_key branch (`handler.go:3486`), which already runs
  `RefreshAccountInfo` in a goroutine.

Change both to probe the one key: call `refreshApiKeyInfoWithRegionProbe(account, req.Region)`
and set `account.Region = detected` on HIT **before** persisting. On probe failure, fall back
to `req.Region` (or `us-east-1`). For `apiImportCredentials`, fold the probe into the existing
async `RefreshAccountInfo` goroutine so import latency doesn't block the response (probe is
still best-effort; the account is persisted immediately).

### 6. Frontend — `web/app.js` + `web/index.html`

Make the region input **optional** (empty = auto-detect) in all three api_key entry points:

- **Single add-account modal** (`modalApiKey`, `app.js:2750`): `<input id="apiKeyRegion">` —
  remove `value="us-east-1"`, add `placeholder` from new i18n `apikey.regionAuto`. `importApiKey`
  (`app.js:2971`): send `region` as-is (empty when blank → backend auto-detects); drop the
  `|| 'us-east-1'` fallback.
- **Add-account batch modal** (`modalApiKeyBatch`, `app.js:2764`): same change to
  `apiKeyBatchRegion`; `importApiKeysBatch` (`app.js:2983`) drops the `|| 'us-east-1'`.
- **Admin B4 import modal** (`importApiKeysSubmit`, `app.js:2329`): currently hardcodes
  `region: 'us-east-1'` with no input. Change the POST body to **omit** `region` (backend
  treats empty/absent as auto-detect). No new input field added (keep this paste-JSON modal
  minimal); auto-detect is the new default behavior.

### 7. i18n — `web/locales/en.json` + `web/locales/zh.json`

Add to both (en/zh), additive:
- `apikey.regionAuto` — en `"Auto-detect (leave empty)"` / zh `"自动识别（留空）"`
- `apikey.regionHint` — en `"Leave empty to auto-detect the region by probing. Type a region to try it first."` / zh equivalent.

(Label reuses existing `detail.region` = "Region" in both locales.)

## Security

No new secret handling. `getUsageLimitsForRegion` is a read-only GET using the api_key as
bearer (already done by `GetUsageLimits` today). Probing sends the same key to N regional
hosts instead of one — acceptable: the key already trusts AWS Q, and these are all official
`q.<region>.amazonaws.com` hosts. No cleartext key is logged (existing masking preserved).
No new admin route (reuse `/auth/apikeys-batch`, `/auth/credentials`, `/admin/api/accounts`,
all already behind existing auth).

## Non-breaking JSON

No config-struct changes. Region detection mutates only the created `Account.Region` at
import time (already a stored field). Env override `KIRO_APIKEY_REGIONS` is new and optional.

## Edge cases / trade-offs

- **Bad key:** probe stops on the first 401/403 (usually `us-east-1`, the first candidate),
  so a bad key does NOT trigger a full 12-region sweep — fast fail.
- **Mixed-region batch:** only the first key's region is detected. Keys in other regions
  404 → `InfoFailed`, created anyway. Chosen trade-off (probe-once) — accurate for the common
  same-region batch; re-probe-per-key (hybrid) is explicitly deferred.
- **Probe fully fails (all 404):** account created with hint/default `us-east-1`. Chat path
  remains the authority for api_key validity (`handleAccountFailure` bans on real 403), so a
  wrong-region account still gets caught at first chat — no silent breakage.
- **Existing accounts unchanged:** no re-probe on existing accounts; only import paths change.

## Testing

No injected seam: the loop calls the real `GetUsageLimits` on each per-region copy. The
region is encoded in the request host (`codewhisperer.us-east-1.amazonaws.com` for us-east-1,
`q.<region>.amazonaws.com` otherwise), so a unit test stubs `kiroRestHttpStore` with a
`roundTripFunc` that inspects `req.URL.Host` and returns 200 for the target region's host /
404 for the others — exercising the real region-resolution + host-rewrite path. This matches
the existing `apikey_batch_test.go` and `kiro_region_test.go` conventions (no function-injection
seam exists anywhere in the package).

- Unit: `apiKeyRegionCandidates` — hint-first ordering, dedup, `KIRO_APIKEY_REGIONS` override.
- Unit: `apiKeyProbeFatal` — 401/403/402 → true; 404/500/network → false.
- Unit: `refreshApiKeyInfoWithRegionProbe` (stubbed `kiroRestHttpStore`) — HIT returns (usage, region, nil); 404 → continues to next region; 401/403/402 → stops early (only one host hit); all-404 → last error. Assert the input account is not mutated (its Region/ApiRegion unchanged).
- Unit: `ImportApiKeys` (stubbed `kiroRestHttpStore`, 200 for `q.eu-central-1.amazonaws.com` only) — persisted accounts get `Region == "eu-central-1"`; probe makes one sweep regardless of batch size.
- Build + smoke: single-import wiring (`apiImportCredentials`, `apiAddAccount`) against a real upstream.

## Files touched

| File | Change |
|---|---|
| `proxy/kiro_api.go` | `defaultApiKeyRegions`, `apiKeyRegionCandidates`, `apiKeyProbeFatal`, `refreshApiKeyInfoWithRegionProbe`, `refreshApiKeyAccountWithRegionDetection` (single-import helper) — reuses existing `GetUsageLimits` |
| `proxy/apikey_batch.go` | `ImportApiKeys`: split into parse/dedup pass → probe-once-per-batch → create pass |
| `proxy/handler.go` | `apiImportCredentials` + `apiAddAccount` api_key branches: call `refreshApiKeyAccountWithRegionDetection` async |
| `web/app.js` | region inputs optional (single `modalApiKey`, batch `modalApiKeyBatch`); B4 `importApiKeysSubmit` omits `region` |
| `web/locales/en.json`, `web/locales/zh.json` | `apikey.regionAuto`, `apikey.regionHint` |
| `proxy/kiro_api_region_test.go` (new) + `proxy/apikey_batch_test.go` | unit tests: candidates, fatal classifier, probe loop (stubbed store), ImportApiKeys region detection |
