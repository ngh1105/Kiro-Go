# Test Plan: effort Support (output_config.effort → inferenceConfig.effort)

Status: **blocked — needs Kiro account.** Run when an account is available.

## Background

Effort was dropped end-to-end. Fixed in:

- `proxy/translator.go` — `ClaudeRequest.OutputConfig` parse, `effortCapableModels` allowlist (DOT form), `normalizeEffort` / `modelSupportsEffort` / `clampEffortForModel` / `resolveEffort` / `resolveEffortValue` / `mapOpenAIReasoningEffort` / `advertiseModelID`.
- `proxy/translator.go:344` — `ClaudeToKiro` forwards `inferenceConfig.effort`.
- `proxy/translator.go:1292` — `OpenAIToKiro` forwards `reasoning_effort` → effort.
- `proxy/kiro.go` — `InferenceConfig.Effort` field (`effort,omitempty`).
- `proxy/handler.go` — advertise **DASH** form (`claude-opus-4-8`, `claude-sonnet-5`, …) + `advertiseModelID` on cached list.

`effortCapableModels` (DOT form, what `MapModel` outputs upstream):
`claude-opus-4.8`, `claude-opus-4.7`, `claude-opus-4.6`, `claude-opus-4.5`, `claude-sonnet-5`, `claude-sonnet-4.6`.

Effort dropped on: `claude-sonnet-4.5`, `claude-haiku-4.5`, older.
Opus 4.5 clamps `xhigh`/`max` → `high` (rejects higher levels).

Build + `go vet ./proxy/` pass. Unit tests not yet written.

## Effort facts (claude-api skill)

- Field: `output_config.effort` — nested in `output_config`, **not** top-level. GA, no beta header.
- Values: `low` | `medium` | `high` | `xhigh` | `max`. Default `high`.
- Model id **dash** required (`claude-opus-4-8`). Dot form → client doesn't recognize model → no effort sent.

## Pre-flight (no account)

- [ ] `go build ./...` clean.
- [ ] `go vet ./proxy/` clean.
- [ ] Confirm `/v1/models` advertises dash form: `curl -s localhost:PORT/v1/models | grep claude-opus-4-8` (expect dash, not dot). Proxy must be running.

## Unit tests to add (`proxy/translator_test.go`)

1. `advertiseModelID("claude-opus-4.8")` → `"claude-opus-4-8"`.
2. `advertiseModelID("claude-sonnet-5")` → `"claude-sonnet-5"` (already dash, unchanged).
3. `normalizeEffort`: `""`/`"  HIGH  "`/`"GARBAGE"` → `""`/`"high"`/`""`.
4. `modelSupportsEffort`: opus 4.8/4.7/4.6/4.5 + sonnet-5/4.6 → true; sonnet 4.5/haiku → false. fable-5 → false (not in Kiro).
5. `clampEffortForModel("xhigh","claude-opus-4.5")` → `"high"`; same on opus 4.8 → `"xhigh"`.
6. `resolveEffort`:
   - nil OutputConfig → `""`
   - opus 4.8 + `high` → `"high"`
   - sonnet 4.5 + `high` → `""` (dropped)
   - opus 4.5 + `max` → `"high"` (clamped)
   - garbage value → `""`
7. `mapOpenAIReasoningEffort`: `"minimal"`→`"low"`, `"high"`→`"high"`, `""`/unknown → `""`.
8. `ClaudeToKiro` end-to-end: `ClaudeRequest{Model:"claude-opus-4-8", OutputConfig:{Effort:"high"}}` → payload `InferenceConfig.Effort == "high"` AND `InferenceConfig.ModelID`/`userInputMessage.ModelID == "claude-opus-4.8"` (dash→dot round-trip).
9. `OpenAIToKiro` end-to-end: `OpenAIRequest{Model:"claude-opus-4-8", ReasoningEffort:"high"}` → `InferenceConfig.Effort == "high"`.
10. Effort-only request (no max_tokens/temp/topP) still constructs `InferenceConfig` when model supports effort; `InferenceConfig == nil` when effort dropped (e.g. sonnet 4.5).

## Runtime tests (needs account)

Proxy running, valid account imported. Capture the Kiro upstream payload via debug log or mitmproxy/HAR.

### T1 — Claude Code sends effort

1. Point Claude Code at proxy, select `claude-opus-4-8`.
2. Send a request.
3. **Verify** client request body contains `"output_config":{"effort":...}`. If absent → client didn't recognize model (still dot form somewhere) → check `advertiseModelID` path + cached list.
4. **Verify** Kiro upstream payload `inferenceConfig.effort` populated, `userInputMessage.modelId == "claude-opus-4.8"` (dot).

### T2 — Effort levels forwarded

For each of `low`/`medium`/`high`/`xhigh`/`max` on `claude-opus-4-8`:
- Kiro upstream `inferenceConfig.effort` matches level.
- Response returns (no upstream 400).

### T3 — Drop on unsupported model

`claude-sonnet-4-5` (or haiku) with `output_config.effort:"high"`:
- Kiro payload has **no** `inferenceConfig.effort` (dropped by `resolveEffort`).
- Request succeeds (no 400 from upstream rejecting effort on unsupported model).

### T4 — Opus 4.5 clamp

`claude-opus-4-5` with `effort:"xhigh"`:
- Kiro payload `inferenceConfig.effort == "high"` (clamped).

### T5 — OpenAI path

`POST /v1/chat/completions` with `"reasoning_effort":"high"`, `model:"claude-opus-4-8"`:
- Kiro payload `inferenceConfig.effort == "high"`.

### T6 — Effort actually changes behavior (optional, qualitative)

Same prompt on opus 4.8 at `low` vs `max`:
- `max` should show more thinking / longer output; `low` terser. Confirms effort reaches the model, not just the payload. Subjective — log if inconclusive.

## Known gaps / follow-ups

- fable-5 not supported (Kiro upstream doesn't serve it). If Kiro adds fable-5 later, add `"claude-fable-5"` to `effortCapableModels` + fallback list.
- No structured-outputs (`output_config.format`) forwarding — only effort.
- Effort on history/priming messages: only `currentMessage` modelID gates; effort is on top-level `inferenceConfig` (covers whole request), so this is fine.
