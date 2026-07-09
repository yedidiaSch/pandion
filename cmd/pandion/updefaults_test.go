// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/userconfig"
)

func TestApplyUpDefaults(t *testing.T) {
	const flagTTL = 45 * time.Minute // stand-in for the flag's built-in default
	def := userconfig.Defaults{Region: "nbg1", Size: "cpx21", TTL: "2h"}

	tests := []struct {
		name                 string
		size, region         string
		ttl                  time.Duration
		ttlSet, noTTL        bool
		d                    userconfig.Defaults
		wantSize, wantRegion string
		wantTTL              time.Duration
		wantWarn             bool
	}{
		{
			name: "all-unset takes every config default",
			ttl:  flagTTL, d: def,
			wantSize: "cpx21", wantRegion: "nbg1", wantTTL: 2 * time.Hour,
		},
		{
			name: "explicit flags win over config",
			size: "cx11", region: "fsn1", ttl: 10 * time.Minute, ttlSet: true, d: def,
			wantSize: "cx11", wantRegion: "fsn1", wantTTL: 10 * time.Minute,
		},
		{
			name: "no config leaves flags untouched (auto-select)",
			ttl:  flagTTL, d: userconfig.Defaults{},
			wantSize: "", wantRegion: "", wantTTL: flagTTL,
		},
		{
			name: "no-ttl suppresses the ttl default but not size/region",
			ttl:  flagTTL, noTTL: true, d: def,
			wantSize: "cpx21", wantRegion: "nbg1", wantTTL: flagTTL,
		},
		{
			name: "invalid config ttl warns and keeps the flag ttl",
			ttl:  flagTTL, d: userconfig.Defaults{TTL: "banana"},
			wantSize: "", wantRegion: "", wantTTL: flagTTL, wantWarn: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs, gr, gt, warn := applyUpDefaults(tc.size, tc.region, tc.ttl, tc.ttlSet, tc.noTTL, tc.d)
			if gs != tc.wantSize || gr != tc.wantRegion || gt != tc.wantTTL {
				t.Errorf("got size=%q region=%q ttl=%v; want size=%q region=%q ttl=%v",
					gs, gr, gt, tc.wantSize, tc.wantRegion, tc.wantTTL)
			}
			if (warn != "") != tc.wantWarn {
				t.Errorf("warn=%q, wantWarn=%v", warn, tc.wantWarn)
			}
		})
	}
}
