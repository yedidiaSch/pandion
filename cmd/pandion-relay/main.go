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
	"golang.org/x/crypto/acme/autocert"
)

func main() {
	addr := flag.String("addr", ":8443", "TLS listen address")
	spool := flag.String("spool", "/var/lib/pandion-relay/sessions", "session spool directory")
	dir := flag.String("state", "/var/lib/pandion-relay", "state dir for certs")
	hosts := flag.String("hosts", "", "comma-separated IPs/DNS names for the self-signed cert")
	domain := flag.String("domain", "", "public DNS name for automatic Let's Encrypt TLS (implies :443, TLS-ALPN-01)")
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

	rsrv := relay.NewServer(store, relay.SSHDialer)
	rsrv.RecordDir = filepath.Join(*dir, "recordings")
	srv := &http.Server{
		Addr:              *addr,
		Handler:           rsrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if *domain != "" {
		// Automatic, browser-trusted TLS via Let's Encrypt. TLS-ALPN-01 is answered
		// on the same :443 listener (no port 80 needed). The node's firewall opens
		// :443 and the systemd unit grants CAP_NET_BIND_SERVICE.
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(*domain),
			Cache:      autocert.DirCache(filepath.Join(*dir, "acme")),
		}
		srv.TLSConfig = m.TLSConfig()
		log.Printf("pandion-relay on %s for %s (Let's Encrypt)", *addr, *domain)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("serve: %v", err)
			os.Exit(1)
		}
		return
	}

	// Default: self-signed TLS (the operator/participant verifies the fingerprint).
	certPath := filepath.Join(*dir, "relay.crt")
	keyPath := filepath.Join(*dir, "relay.key")
	fp, err := relay.EnsureSelfSigned(certPath, keyPath, splitCSV(*hosts))
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	log.Printf("pandion-relay on %s  (TLS SHA-256 %s)", *addr, fp)
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
