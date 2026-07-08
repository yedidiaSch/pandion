// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"strings"
)

// Referral / signup suggestions.
//
// When a user has no API token for a provider, Pandion offers a friendly pointer
// to open an account. For DigitalOcean we can use a REFERRAL link once we have a
// code — but only ever with a clear affiliate DISCLOSURE (FTC + trust). Until a
// code is configured we still help newcomers with the provider's plain signup
// page, with no referral claim.
//
// The code is intentionally empty here; fill doRefcode when the affiliate link is
// issued, or set PANDION_DO_REFCODE at runtime (handy for testing the referral
// path before baking the code in).
const doRefcode = "" // e.g. "abc123def456" — DigitalOcean refcode; empty = no referral yet

// doSignupURL is the non-referral fallback: DigitalOcean's public signup page.
const doSignupURL = "https://www.digitalocean.com/try/free-trial-offer"

// doReferralURL builds the referral URL for a code (DigitalOcean's ?refcode form).
func doReferralURL(refcode string) string {
	return "https://www.digitalocean.com/?refcode=" + refcode
}

// resolveDORefcode returns the active DigitalOcean refcode: the env override wins
// (so the referral path can be exercised without a rebuild), else the baked const.
func resolveDORefcode() string {
	if c := strings.TrimSpace(os.Getenv("PANDION_DO_REFCODE")); c != "" {
		return c
	}
	return strings.TrimSpace(doRefcode)
}

// signupSuggestion returns a "you don't have an account yet" pointer for a provider,
// or "" if we have no suggestion for it. When a referral code is present it uses the
// referral URL, mentions the sign-up credit, and DISCLOSES that it is a referral
// link; otherwise it points at the plain signup page with no referral claim. Pure
// (refcode passed in) so it is straightforward to unit-test both branches.
func signupSuggestion(provider, refcode string) string {
	switch provider { // canonical name or the `do` alias
	case "digitalocean", "do":
	default:
		return "" // no referral/signup pointer configured for this provider (yet)
	}
	if refcode = strings.TrimSpace(refcode); refcode != "" {
		return "New here? No DigitalOcean account yet? Get $200 in free credit to try Pandion:\n" +
			"  " + doReferralURL(refcode) + "\n" +
			"  (referral link — helps support Pandion's development)"
	}
	return "New here? No DigitalOcean account yet? Create one at:\n" +
		"  " + doSignupURL
}

// printSignupSuggestion writes the provider's signup suggestion (if any) to stderr.
func printSignupSuggestion(provider string) {
	if s := signupSuggestion(provider, resolveDORefcode()); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}
}
