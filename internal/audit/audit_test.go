// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestEvent_WritesStructuredJSON(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, slog.LevelInfo)
	Event("provision", "id", "pipeline", "node", "broker", "ip", "1.2.3.4")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "provision" || rec["id"] != "pipeline" || rec["node"] != "broker" || rec["ip"] != "1.2.3.4" {
		t.Fatalf("event fields wrong: %+v", rec)
	}
	if _, ok := rec["time"]; !ok {
		t.Error("slog should stamp a time")
	}
}

// Below the configured level, nothing is written.
func TestEvent_LevelFilter(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf, slog.LevelError) // Event() logs at Info, which is below Error
	Event("provision", "id", "x")
	if buf.Len() != 0 {
		t.Fatalf("Info event should be filtered at Error level, got: %s", buf.String())
	}
}

func TestLevelFromEnv(t *testing.T) {
	cases := map[string]struct {
		lvl slog.Level
		on  bool
	}{
		"":      {slog.LevelInfo, false},
		"debug": {slog.LevelDebug, true},
		"info":  {slog.LevelInfo, true},
		"warn":  {slog.LevelWarn, true},
		"error": {slog.LevelError, true},
		"junk":  {slog.LevelInfo, true}, // set-but-unknown ⇒ info, enabled
	}
	for v, want := range cases {
		if v == "" {
			os.Unsetenv("PANDION_LOG")
		} else {
			os.Setenv("PANDION_LOG", strings.ToUpper(v)) // case-insensitive
		}
		lvl, on := LevelFromEnv()
		if lvl != want.lvl || on != want.on {
			t.Errorf("PANDION_LOG=%q => (%v,%v), want (%v,%v)", v, lvl, on, want.lvl, want.on)
		}
	}
	os.Unsetenv("PANDION_LOG")
}
