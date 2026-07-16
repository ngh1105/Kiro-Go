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
