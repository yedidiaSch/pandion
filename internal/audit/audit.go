// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audit emits a structured (JSON) trail of Pandion's OWN infrastructure
// actions — provision, teardown, lockdown, reap — for debugging Pandion itself
// and as an after-the-fact record (L3). It is deliberately separate from the
// human console output and from the workload's multiplexed streams: those stay
// readable, this is machine-parseable and quiet by default.
//
// Enable a live copy to stderr with PANDION_LOG (debug|info|warn|error). The
// trail is always appended to ~/.pandion/logs/audit.jsonl once Init runs.
package audit

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// noop discards until Init configures a real destination.
var current atomic.Pointer[slog.Logger]

func init() {
	l := slog.New(slog.NewJSONHandler(io.Discard, nil))
	current.Store(l)
}

// Init points the audit logger at w, emitting JSON lines at `level` and above.
func Init(w io.Writer, level slog.Level) {
	current.Store(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})))
}

// LevelFromEnv parses PANDION_LOG (debug|info|warn|error). Returns the level and
// whether the variable was set at all (set ⇒ also echo the trail to stderr).
func LevelFromEnv() (slog.Level, bool) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PANDION_LOG")))
	if v == "" {
		return slog.LevelInfo, false
	}
	switch v {
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default: // "info" or anything else set
		return slog.LevelInfo, true
	}
}

// Event records one infra action with structured key/value attributes, e.g.
//
//	audit.Event("provision", "id", id, "node", name, "ip", ip)
func Event(action string, kv ...any) {
	current.Load().Info(action, kv...)
}
