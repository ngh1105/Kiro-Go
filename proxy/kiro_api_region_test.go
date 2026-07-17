package proxy

import (
	"errors"
	"io"
	"net/http"
	"strings"
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

// TestRefreshApiKeyInfoWithRegionProbeHit walks candidates until the eu-central-1
// host returns 200, and asserts the input account is NOT mutated by the probe.
func TestRefreshApiKeyInfoWithRegionProbeHit(t *testing.T) {
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
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
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
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
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

// TestRefreshApiKeyAccountWithRegionDetectionPersistsRegion verifies the
// single-import wrapper detects a new region, persists it via config.UpdateAccount,
// returns regionChanged=true, and leaves the in-memory account updated. Only the
// eu-central-1 host returns 200; us-east-1 (the account's starting region) 404s.
func TestRefreshApiKeyAccountWithRegionDetectionPersistsRegion(t *testing.T) {
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

	acct := config.Account{ID: "ak-1", AuthMethod: "api_key", KiroApiKey: "KEY-1", AccessToken: "KEY-1", Region: "us-east-1", Enabled: true}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, regionChanged, err := refreshApiKeyAccountWithRegionDetection(&acct)
	if err != nil {
		t.Fatalf("expected success, got err %v", err)
	}
	if !regionChanged {
		t.Fatal("regionChanged: want true, got false")
	}
	if acct.Region != "eu-central-1" || acct.ApiRegion != "eu-central-1" {
		t.Fatalf("in-memory account region not updated: Region=%q ApiRegion=%q", acct.Region, acct.ApiRegion)
	}
	for _, a := range config.GetAccounts() {
		if a.ID == "ak-1" {
			if a.Region != "eu-central-1" {
				t.Fatalf("persisted Region: want eu-central-1, got %q", a.Region)
			}
			return
		}
	}
	t.Fatal("seeded account ak-1 not found in persisted store")
}

// TestRefreshApiKeyAccountWithRegionDetectionSeedsProbeUsage pins the optimization:
// when the probe succeeds, the wrapper must parse the probe's already-fetched
// usage instead of re-issuing GetUsageLimits against the detected region. Only
// eu-central-1 returns 200; the stub counts eu-central-1 host hits. Seeded = 1
// (probe only); not seeded = 2 (probe + RefreshAccountInfo).
func TestRefreshApiKeyAccountWithRegionDetectionSeedsProbeUsage(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var euCentral1Hits int
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "q.eu-central-1.amazonaws.com" {
				euCentral1Hits++
			}
			code, body := http.StatusNotFound, "not found"
			if req.URL.Host == "q.eu-central-1.amazonaws.com" {
				code, body = 200, `{"userInfo":{"email":"probe@example.com","userId":"u1"}}`
			}
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	acct := config.Account{ID: "ak-seed", AuthMethod: "api_key", KiroApiKey: "KEY-SEED", AccessToken: "KEY-SEED", Region: "us-east-1", Enabled: true}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("seed: %v", err)
	}

	info, _, err := refreshApiKeyAccountWithRegionDetection(&acct)
	if err != nil {
		t.Fatalf("expected success, got err %v", err)
	}
	if euCentral1Hits != 1 {
		t.Fatalf("eu-central-1 host hits: want 1 (probe seeds usage, no re-fetch), got %d", euCentral1Hits)
	}
	if info == nil || info.Email != "probe@example.com" {
		t.Fatalf("info.Email: want probe@example.com (parsed from probe usage), got %+v", info)
	}
}
