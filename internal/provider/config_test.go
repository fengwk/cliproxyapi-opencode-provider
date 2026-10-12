package provider

import "testing"

// TestValidateBaseURLRejectsForceQuery pins the trailing-? query form. A bare
// "?" leaves RawQuery empty but sets ForceQuery, so a URL that would otherwise
// push the appended endpoint into the query string must be rejected.
func TestValidateBaseURLRejectsForceQuery(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/v1?",
		"https://example.com/v1?x=1",
		"https://example.com/v1#frag",
	} {
		if got, err := validateBaseURL(raw); err == nil {
			t.Errorf("validateBaseURL(%q) = %q, want error", raw, got)
		}
	}

	// A normal URL stays unchanged, and the joined endpoint keeps its path.
	for _, raw := range []string{"https://example.com/v1", "http://127.0.0.1:8080"} {
		got, err := validateBaseURL(raw)
		if err != nil || got != raw {
			t.Fatalf("validateBaseURL(%q) = %q, %v; want unchanged", raw, got, err)
		}
	}
	if joined := joinURL("https://example.com/v1", endpointChat); joined != "https://example.com/v1/chat/completions" {
		t.Fatalf("joinURL = %q", joined)
	}
}
