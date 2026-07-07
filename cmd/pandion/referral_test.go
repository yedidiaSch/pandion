package main

import (
	"strings"
	"testing"
)

func TestSignupSuggestion_ReferralDisclosed(t *testing.T) {
	// With a refcode: referral URL + credit perk + a clear affiliate disclosure.
	for _, prov := range []string{"digitalocean", "do"} {
		s := signupSuggestion(prov, "abc123")
		if !strings.Contains(s, "refcode=abc123") {
			t.Errorf("%s: want referral URL with the code, got:\n%s", prov, s)
		}
		if !strings.Contains(strings.ToLower(s), "referral link") {
			t.Errorf("%s: referral MUST be disclosed, got:\n%s", prov, s)
		}
		if !strings.Contains(s, "$200") {
			t.Errorf("%s: expected the sign-up credit perk, got:\n%s", prov, s)
		}
	}
}

func TestSignupSuggestion_NoRefcode_PlainSignupNoDisclosure(t *testing.T) {
	// Without a refcode: plain signup page, and NO "referral" claim (it isn't one).
	s := signupSuggestion("digitalocean", "")
	if s == "" {
		t.Fatal("expected a plain signup pointer when no refcode is set")
	}
	if strings.Contains(strings.ToLower(s), "referral") {
		t.Errorf("must NOT claim referral when there is no code, got:\n%s", s)
	}
	if !strings.Contains(s, doSignupURL) {
		t.Errorf("expected the plain signup URL, got:\n%s", s)
	}
	// whitespace-only code is treated as no code.
	if signupSuggestion("digitalocean", "  ") != s {
		t.Error("a blank refcode should behave like no refcode")
	}
}

func TestSignupSuggestion_UnknownProviderEmpty(t *testing.T) {
	for _, prov := range []string{"hetzner", "vultr", "linode", "scaleway", "mock", ""} {
		if got := signupSuggestion(prov, "abc123"); got != "" {
			t.Errorf("provider %q should have no suggestion, got: %q", prov, got)
		}
	}
}

func TestResolveDORefcode_EnvOverridesConst(t *testing.T) {
	t.Setenv("PANDION_DO_REFCODE", "envcode99")
	if got := resolveDORefcode(); got != "envcode99" {
		t.Fatalf("env override should win, got %q", got)
	}
}
