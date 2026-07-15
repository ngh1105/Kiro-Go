package proxy

// Outbound webhook notifications for operational events (account ban, key
// over-limit). Best-effort, async, never blocks the request path.
//
// SECURITY: the payload is constructed entirely by the caller from safe fields
// (account id/email, api key id/name, event reason). This helper has no access
// to cleartext keys/tokens and does not import any config secret accessor other
// than GetWebhookURL (the destination). No key/token value ever appears in a
// webhook body.
import (
	"bytes"
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"sync"
	"time"
)

// notifyWebhook fires a best-effort async POST to the configured webhook URL.
// Empty URL → immediate no-op. The HTTP call runs in a goroutine with a 5s
// client timeout. Failures are logged at warn; panics are recovered and logged.
// Callers must never pass key/token values in payload.
func notifyWebhook(event string, payload map[string]interface{}) {
	url := config.GetWebhookURL()
	if url == "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warnf("[Webhook] panic (event=%s): %v", event, r)
			}
		}()
		body := map[string]interface{}{
			"event":   event,
			"time":    time.Now().Unix(),
			"payload": payload,
		}
		b, err := json.Marshal(body)
		if err != nil {
			logger.Warnf("[Webhook] marshal failed (event=%s): %v", event, err)
			return
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(url, "application/json", bytes.NewReader(b))
		if err != nil {
			logger.Warnf("[Webhook] POST failed (event=%s): %v", event, err)
			return
		}
		_ = resp.Body.Close()
	}()
}

// over-limit webhook dedup. A key that stays over quota gets retried on every
// request (clients commonly retry on 429); without a cooldown each retry would
// spawn a goroutine + webhook POST, exhausting goroutines/connections under
// sustained load and flooding the webhook with duplicate events. Cool to at
// most one "key.over_limit" notification per key per window. Bounded by the
// number of keys (one map entry per key ever over limit).
var (
	overLimitNotifyMu   sync.Mutex
	overLimitNotifyLast = make(map[string]time.Time)
)

const overLimitNotifyCooldown = 60 * time.Second

// shouldNotifyOverLimit reports whether an over-limit webhook for apiKeyID
// should fire now (true) or is still on cooldown (false), recording the time.
// An empty apiKeyID (legacy single-key path has no per-entry id) always fires.
func shouldNotifyOverLimit(apiKeyID string, now time.Time) bool {
	if apiKeyID == "" {
		return true
	}
	overLimitNotifyMu.Lock()
	defer overLimitNotifyMu.Unlock()
	if last, ok := overLimitNotifyLast[apiKeyID]; ok && now.Sub(last) < overLimitNotifyCooldown {
		return false
	}
	overLimitNotifyLast[apiKeyID] = now
	return true
}
