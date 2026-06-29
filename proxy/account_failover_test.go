package proxy

import (
	"errors"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"testing"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

// TestHandleAccountFailureDoesNotBanOnForbiddenInBody verifies the request-path
// error classifier does NOT permanently ban a healthy account when a NON-auth
// upstream error (e.g. a 502 gateway / CloudFront HTML page) merely contains the
// word "forbidden" in its body. The old isAuthErrorMessage ran bare
// strings.Contains over the full error string (which embeds the upstream body)
// and false-banned healthy accounts. Routing through pool.IsAuthFailure matches
// status codes by digit boundary and uses curated markers, so a body word with no
// 401/403 token and no auth marker must not ban.
func TestHandleAccountFailureDoesNotBanOnForbiddenInBody(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// "forbidden" appears only as a body word — NO 401/403 status token, NO auth
	// marker. Bare substring matching banned this; pool.IsAuthFailure must not.
	h.handleAccountFailure(&acc, errors.New("upstream returned 502: <html>nginx error: access forbidden</html>"))

	got, _ := config.GetAccountByID("acct")
	if !got.Enabled || got.BanStatus != "" {
		t.Fatalf("account should NOT be banned for a forbidden-in-body non-auth error; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}

// TestHandleAccountFailureBansOnGenuineAuthError verifies a genuine auth failure
// (401 status) still permanently bans the account after routing through
// pool.IsAuthFailure.
func TestHandleAccountFailureBansOnGenuineAuthError(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, _ := config.GetAccountByID("acct")

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	h.handleAccountFailure(&acc, errors.New("upstream error (status 401): unauthorized"))

	got, _ := config.GetAccountByID("acct")
	if got.Enabled || got.BanStatus != "BANNED" {
		t.Fatalf("genuine auth error should ban the account; got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
}
