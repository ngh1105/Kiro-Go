# Kiro-Go Admin Panel — Enhancement Spec

> Gom chung tất cả đề xuất mở rộng UI/admin panel thành 1 tài liệu. Phục vụ việc
> triển khai theo từng đợt. Trạng thái "hiện tại" được verify từ source (index.html,
> app.js, handler.go, admin_apikeys.go, auth.go) — không đoán.

Status legend: ✅ đã có | 🟡 có một phần / mỏng | 🔴 chưa có

---

## 1. Mục tiêu

1. **Hiển thị đầy đủ token flow** trong Logs: input / output / cache-read / cache-write
   cho mỗi request + tổng hợp (hit-rate, cache tokens) ở cấp tổng.
2. **Kiểm soát API key chặt hơn**: xem lại key đã tạo, tìm/lọc/xuất, thao tác hàng loạt,
   thống kê dùng từng key (RPM/TPM/credits/last-used) và breakdown theo model.
3. **Mở rộng 4 tab còn lại** (Accounts / Settings / API / Logs) cho đủ nhu cầu vận hành
   (series thời gian, breakdown model/key, khóa IP, export logs…).

Nguyên tắc: thêm feature phải kèm i18n **en + zh**, không phá JSON field cũ (thêm field,
không đổi tên field đã có), và không ghi token thật ra bất kỳ output nào.

---

## 2. Status quo (đã verify)

### Stats grid (top)
4 thẻ: Accounts (+credits), Requests (+tokens), Success (completed), Failed (errors).
Đơn giá trị tức thời, **không có series thời gian**. 🟡

### Tab Accounts
- Toolbar: privacy toggle, Export, Refresh-all-models, Add.
- Batch bar: enable / disable / refresh / refreshModels / delete.
- Filter: search email/nickname, status filter.
- Mỗi account: refresh / detail / copy-JSON / disable / test / delete +
  thanh main-quota, requests, tokens, credits, expiry. ✅ khá đầy đủ.

### Tab Settings (8 card)
API Settings (requireApiKey + danh sách key) · Usage Settings (allowOverUsage +
maxPayloadBytes) · Thinking Settings (suffix + format) · Endpoint Settings (preferred +
fallback) · Admin Password · Proxy Settings · Prompt Filter · Statistics. ✅

### Tab API
5 endpoint (claude / openai / openai-responses / models / stats), mỗi cái có
copy / view. ✅ (chỉ là tham chiếu tĩnh).

### Tab Logs
- Filter all/success/error, refresh, auto-refresh 5s, clear.
- Summary (total/success/errors).
- Bảng: time / status / endpoint / model / account / **tokens (In/Out + cache line khi ≠0)** / duration / detail.
- Backend `RequestLog` đã có `inputTokens/outputTokens/cacheRead/cacheCreation/credits/duration`
  (commit `8fe4685`). ✅ in/out/cache đã plumbing.
- In-memory ring 500 entry — **mất khi restart, không persist**. 🟡
- Cache chỉ hiện khi ≠0; không có cache hit-rate, không lọc theo key/model/endpoint. 🟡

### Backend (liên quan spec)
- `auth.go:74-79` → 429 khi `tokenLimit`/`creditLimit` vượt (limit **đã enforce**). ✅
- `recordSuccessForApiKey` (handler.go:1461) → per-key `tokensUsed/creditsUsed/requestsCount/lastUsedAt`. ✅
- `apiKeyView` (admin_apikeys.go) expose `TokenLimit/CreditLimit/TokensUsed/CreditsUsed/RequestsCount/LastUsedAt`. ✅
- Cache tracker (`promptCacheUsage`) đã tính `CacheCreationInputTokens/CacheReadInputTokens` + `cache_metrics.go` tổng hợp. ✅
- `AuditEntry` (persist.go) = jsonl riêng (chỉ method/path/status/latency), **không có token**, không show UI. 🟡

---

## 3. Đề xuất theo vùng

### Vùng A — Logs (token in/out/cache + mở rộng)

**A1. Cột cache luôn có chỗ + hit-rate** 🔴
- Giữ `tokensCell` nhưng thêm 1 dòng "cache" rõ ràng (Cache read / Cache write) ngay cả
  khi 0 → hiển thị `0`, để người dùng thấy cache đang "đắp" hay chưa (hiện tại ẩn khi 0).
- Tùy chọn: gộp cột In/Out/Cache vào 1 popover/tooltip khi cần gọn bảng.

**A2. Summary thêm cache aggregate** 🟡
- `logsSummary` hiện chỉ total/success/errors. Thêm:
  - Total input / total output / total cache-read / total cache-write.
  - **Cache hit-rate** = cache-read / (input + cache-read) trên tập đang xem.
- Nguồn: client tính từ ring (đã có field), hoặc thêm 1 endpoint `/admin/api/logs/summary`.

**A3. Filter đa chiều** 🔴
- Lọc theo: **key (apiKey id/name)**, **model**, **endpoint**, **account**, khoảng time.
- Cần: backend truyền thêm `apiKeyId` vào `RequestLog` (hiện KHÔNG có — `recordSuccessLog`
  không nhận apiKeyId). → thêm field `ApiKeyID`/`ApiKeyName` vào RequestLog + thread qua.
- UI: thêm `<select>` filter bên cạnh filter success/error hiện tại.

**A4. Persist logs (không mất khi restart)** 🟡
- Ring 500 in-memory bay khi restart. Đề xuất: ghi đè `/append` vào `data/` jsonl (tái dùng
  pattern `AuditEntry`) hoặc SQLite (nếu muốn query/filter/sort nặng).
- Ưu tiên thấp nếu A1–A3 đã đủ — đánh giá lại sau khi có filter.

**A5. Export logs (CSV/JSON)** 🔴
- Nút export tập đang filter ra CSV/JSON để audit/troubleshoot.

**A6. Auto-refresh interval tuỳ chỉnh** 🟡
- Hiện fix 5s. Cho chọn 2s/5s/10s/30s.

---

### Vùng B — API Key management (kiểm soát key hơn)

**B1. Bảng key: tìm + lọc + sort** 🔴
- Ô search (name / key prefix).
- Lọc theo enabled/over-limit/expired.
- Sort theo tokensUsed / creditsUsed / requestsCount / lastUsedAt.
- Hiện đã có `apiKeysList` + `apiKeyView` đủ dữ liệu, chỉ thiếu control UI.

**B2. Hiển thị usage trên mỗi key rõ hơn** 🟡
- Mỗi row key: usage bar (tokensUsed/tokenLimit, creditsUsed/creditLimit) dùng lại
  pattern `usageBar()` (app.js:1708) + % + cảnh báo màu khi gần limit.
- Cột RPM/TPM gần-realtime (tính từ ring logs trong 60s/10min). 🟡 cần backend time-bucket.

**B3. Thao tác hàng loạt trên key** 🔴
- Batch: enable / disable / delete / reset-usage (đã có `apiResetApiKeyUsage`).
- Pattern giống batch accounts hiện có (`batchBar`). Cần select-all + checkbox từng key.

**B4. Export / import key (không lộ cleartext khi export)** 🔴
- Export: chỉ mask + metadata (id/name/enabled/limits/usage) — **KHÔNG** xuất cleartext key.
- Import: đã có `/auth/apikeys-batch`. UI nút import + paste JSON.

**B5. Breakdown theo model cho từng key** 🔴
- Tab/section trong detail key: tokens/requests phân theo model.
- Cần: backend gom usage theo (apiKeyId, model) → hiện `recordSuccessForApiKey` chỉ sum,
  phải thêm bucket per-model (map[model]usage) + persist.

**B6. Cấp lại / xoay key (rotate)** 🟡
- Nút rotate → sinh key mới, vô hiệu key cũ (giữ history usage). Đã có `apiCreateApiKey`
  trả cleartext 1 lần; rotate = tạo mới + disable cũ.

**B7. IP allowlist / giới hạn mỗi key** 🔴 (tuỳ nhu cầu)
- Mỗi key thêm field `allowedIPs []string`; check ở `authenticate` (auth.go).
- Chỉ làm nếu dùng expose public.

---

### Vùng C — Accounts (mở rộng)

**C1. Cột/section cache per-account** 🟡
- Cache metrics (`cache_metrics.go`) đã có per-account. Hiện account detail chỉ requests/
  tokens/credits. Thêm cache-read/creation + hit-rate vào detail hoặc row.

**C2. Health / latency trend** 🟡
- Per-account: success-rate, latency p50/p95 (cần capture latency, hiện `UpdateStats`
  không lưu latency do dispatch chưa land — xem comment handler.go:1332). Khi dispatch
  land thì bật lại.

**C3. Bulk test / batch refresh token** 🟡
- Đã có batch refresh; thêm "test connectivity" hàng loạt cho N account.

**C4. Tag/nhóm account + filter theo nhóm** 🔴
- Field `tags []string`; filter + batch theo tag. Hữu ích khi pool lớn (hiện 75 account).

---

### Vùng D — Settings (mở rộng)

**D1. Cache tuning** 🟡
- `cacheTracker` có `opusMinCacheableTokens`, cap… Đưa 1 card "Prompt cache" cho chỉnh
  min-tokens / hit-cap / bật-tắt cache (nếu có flag).

**D2. Rate-limit toàn cục** 🔴
- RPM/TPM limit toàn proxy (khác per-key). Card "Rate limiting" + 429 khi vượt.

**D3. Notification / webhook khi account fail / sắp hết quota** 🔴
- Field `webhookUrl`; gửi sự kiện (account disabled, key over-limit).

**D4. Backup/restore config** 🟡
- Export/import `data/config.json` (KHÔNG commit; encrypt token khi export). Đã có
  Export account → mở rộng thành full config snapshot.

**D5. Security checklist card** 🔴
- Hiện boot warn: default password `changeme`, bind `0.0.0.0` không auth. Card tóm tắt
  tình trạng bảo mật + link đổi password/bind/requireApiKey nhanh.

---

### Vùng E — API tab + Stats

**E1. Stats series thời gian** 🟡
- `/admin/api/stats` hiện trả snapshot. Thêm bucket 1m/1h/1d (requests/tokens/errors)
  → sparkline trên stats grid + tab "Analytics".
- Dùng skill `dataviz` khi vẽ chart/dashboard để palette + a11y nhất quán.

**E2. Copy dạng curl example** 🟡
- Mỗi endpoint thêm nút "Copy curl" (curl sample + header mẫu) tiện test.

**E3. Live request stream** 🔴 (tuỳ nhu cầu)
- WebSocket / SSE feed request đang chạy → debug realtime. Lớn, ưu tiên thấp.

---

## 4. Thay đổi backend cụ thể (cross-cutting)

| Việc | File | Chi tiết |
|---|---|---|
| `RequestLog` thêm ApiKey | handler.go:24 | `ApiKeyID string` (+ `ApiKeyName` resolve từ id) |
| Thread apiKeyId vào log | handler.go 6 call site | `recordSuccessLog` nhận thêm apiKeyId |
| Persist logs | persist.go pattern | jsonl hoặc SQLite; reuse `AuditEntry` shape mở rộng |
| Per-(key,model) usage | config.RecordApiKeyUsage | bucket map thay vì sum 1 số |
| Cache summary endpoint | handler.go + cache_metrics.go | `/admin/api/logs/summary` (total in/out/cache + hit-rate) |
| Key search/sort | admin_apikeys.go | server-side filter/sort hoặc client (dữ liệu đã về client) |
| Rate-limit global | mới (middleware) | token-bucket RPM/TPM, 429 |
| IP allowlist per key | auth.go + config | `allowedIPs`, check trong `authenticate` |

## 5. i18n (mọi feature đều thêm en + zh)

Pattern hiện có: `logs.input`, `apiKeys.*`, `settings.*`. Mỗi key mới phải có ở cả
`web/locales/en.json` và `web/locales/zh.json`. Ví dụ dự kiến: `logs.cacheHitRate`,
`apiKeys.search`, `apiKeys.batch.*`, `settings.rateLimit.*`, `analytics.*`.

## 6. Ưu tiên / phasing (đề xuất)

- **P0 (ngay, giá trị cao, rủi ro thấp):** A1 (cache luôn hiển thị), A2 (summary cache +
  hit-rate), B1 (search/filter/sort key), B3 (batch key), B2 (usage bar per key).
  → hầu hết là UI thuần trên dữ liệu đã có, ít động backend.
- **P1 (cần backend vừa):** A3 (filter theo key → thêm ApiKeyID vào RequestLog), B4 (export
  key masked), C1 (cache per-account), E1 (stats series), E2 (copy curl).
- **P2 (backend nặng / tuỳ nhu cầu):** A4 (persist logs), B5 (breakdown per-model), B7 (IP
  allowlist), C2 (latency trend — chờ dispatch), C4 (tags), D2 (global rate-limit), D3
  (webhook), E3 (live stream).

## 7. Ràng buộc bảo mật (bắt buộc với mọi đợt)

- KHÔNG ghi token/key cleartext ra log, export, hay UI trừ 1 lần lúc tạo key (đã có).
- Export key chỉ xuất metadata + masked.
- Mọi endpoint admin mới đi qua middleware password hiện có.
- `data/config.json` tiếp tục gitignored; backup/restore phải mã hoá/token-mask.
