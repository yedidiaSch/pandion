// SPDX-License-Identifier: AGPL-3.0-or-later

// Command pandion-relay is the node-hosted browser-SSH relay: it serves an
// xterm.js terminal over TLS and bridges each authorized token to a host-key-pinned
// SSH PTY on a target node, reached over the WireGuard overlay. It runs as an
// unprivileged user on a designated cluster node and holds only the per-session
// scoped keys the CLI writes to its spool. See docs/relay-design.md.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yedidiaSch/pandion/internal/relay"
)

func main() {
	addr := flag.String("addr", ":8443", "TLS listen address")
	spool := flag.String("spool", "/var/lib/pandion-relay/sessions", "session spool directory")
	dir := flag.String("state", "/var/lib/pandion-relay", "state dir for the self-signed cert")
	hosts := flag.String("hosts", "", "comma-separated IPs/DNS names for the self-signed cert")
	flag.Parse()

	store, err := relay.OpenStore(*spool)
	if err != nil {
		log.Fatalf("open spool: %v", err)
	}
	// periodically sweep expired sessions from the spool.
	go func() {
		for range time.Tick(time.Minute) {
			_, _ = store.Reap(time.Now().UTC())
		}
	}()

	certPath := filepath.Join(*dir, "relay.crt")
	keyPath := filepath.Join(*dir, "relay.key")
	fp, err := relay.EnsureSelfSigned(certPath, keyPath, splitCSV(*hosts))
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	log.Printf("pandion-relay on %s  (TLS SHA-256 %s)", *addr, fp)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           relay.NewServer(store, relay.SSHDialer).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServeTLS(certPath, keyPath); err != nil {
		log.Fatalf("serve: %v", err)
		os.Exit(1)
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
