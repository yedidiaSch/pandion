// SPDX-License-Identifier: AGPL-3.0-or-later

package relay

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

//go:embed assets/xterm.js assets/xterm.css assets/xterm-addon-fit.js
var assetFS embed.FS

//go:embed assets/terminal.html
var terminalHTML string

var termTmpl = template.Must(template.New("terminal").Parse(terminalHTML))

// PTYConn is an interactive session to a target node — an SSH PTY in production,
// or a fake in tests. Read yields terminal output; Write feeds keystrokes.
type PTYConn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(rows, cols int) error
	Close() error
}

// Dialer opens a PTYConn for a session. Production uses SSHDialer (a host-key-pinned
// SSH PTY to the session's target over the overlay); tests inject a fake.
type Dialer func(s *Session) (PTYConn, error)

// badTokenPerMin caps how many unknown-token requests a single source IP may make
// per minute before it is throttled — the token is a bearer secret, so this blunts
// brute-force scanning of the /s/ and /ws/ endpoints.
const badTokenPerMin = 20

// Server serves the browser terminal: a token page and a WebSocket bridged to the
// target node's SSH PTY. It holds no keys itself — each session carries its own
// scoped key, resolved from the Store by the presented token.
type Server struct {
	store *Store
	dial  Dialer
	now   func() time.Time
	lim   *limiter
	// RecordDir, if set, is where sessions with Record=true tee their terminal
	// output (one file per session). Empty disables recording.
	RecordDir string
}

// NewServer builds a relay server over a session store and a target dialer.
func NewServer(store *Store, dial Dialer) *Server {
	return &Server{store: store, dial: dial, now: func() time.Time { return time.Now().UTC() }, lim: newLimiter(badTokenPerMin)}
}

// limiter is a minimal per-IP fixed-window counter (no external dependency) used to
// throttle unknown-token attempts.
type limiter struct {
	mu   sync.Mutex
	hits map[string]int
	max  int
}

func newLimiter(max int) *limiter {
	l := &limiter{hits: map[string]int{}, max: max}
	go func() {
		for range time.Tick(time.Minute) {
			l.mu.Lock()
			l.hits = map[string]int{}
			l.mu.Unlock()
		}
	}()
	return l
}

// allow records a failed attempt from ip and reports whether it is still under the
// per-minute cap.
func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[ip]++
	return l.hits[ip] <= l.max
}

// reject writes a 404 for an unknown token, or 429 if the source IP is over the
// bad-token rate cap. Returns true if it handled the response.
func (srv *Server) reject(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !srv.lim.allow(ip) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	http.NotFound(w, r)
}

// Handler returns the HTTP routes: static assets, the token page, and the WebSocket.
func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/assets/", http.FileServer(http.FS(assetFS)))
	mux.HandleFunc("/s/", srv.handlePage)
	mux.HandleFunc("/ws/", srv.handleWS)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Pandion relay", http.StatusNotFound)
	})
	return mux
}

// handlePage serves the xterm.js terminal for a valid token, else a generic 404
// (no oracle on whether a token ever existed).
func (srv *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/s/")
	s, ok := srv.store.Get(token, srv.now())
	if !ok {
		srv.reject(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = termTmpl.Execute(w, map[string]string{"Node": s.Node, "Token": token})
}

// handleWS validates the token, opens the target PTY, and bridges it to the browser.
func (srv *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/ws/")
	s, ok := srv.store.Get(token, srv.now())
	if !ok {
		srv.reject(w, r)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	// bound the whole session by the grant's expiry.
	ctx, cancel := context.WithDeadline(r.Context(), s.Expiry)
	defer cancel()

	pty, err := srv.dial(s)
	if err != nil {
		c.Close(websocket.StatusInternalError, "target unreachable")
		return
	}
	defer pty.Close()

	var rec io.Writer
	if s.Record && srv.RecordDir != "" {
		_ = os.MkdirAll(srv.RecordDir, 0o700) // best-effort; the CLI pre-creates it
		name := s.ID + "-" + strconv.FormatInt(srv.now().Unix(), 10) + ".log"
		f, ferr := os.OpenFile(filepath.Join(srv.RecordDir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if ferr != nil {
			log.Printf("relay: recording disabled for %s: %v", s.ID, ferr)
		} else {
			rec = f
			defer f.Close()
		}
	}
	srv.pump(ctx, c, pty, s.ReadOnly, rec)
	c.Close(websocket.StatusNormalClosure, "session closed")
}

// pump bridges a WebSocket and a PTY: PTY output -> binary WS frames; browser input
// (binary) -> PTY stdin; browser control (text JSON {"resize":{cols,rows}}) -> resize.
// When readOnly, browser keystrokes are dropped (view-only); output and resize still
// flow so the terminal renders and fits. When rec is non-nil, PTY output is also
// teed to it (session recording).
func (srv *Server) pump(ctx context.Context, c *websocket.Conn, pty PTYConn, readOnly bool, rec io.Writer) {
	c.SetReadLimit(1 << 20)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				if rec != nil {
					_, _ = rec.Write(buf[:n])
				}
				if c.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				c.Close(websocket.StatusNormalClosure, "eof")
				return
			}
		}
	}()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var ctl struct {
				Resize *struct{ Cols, Rows int } `json:"resize"`
			}
			if json.Unmarshal(data, &ctl) == nil && ctl.Resize != nil {
				_ = pty.Resize(ctl.Resize.Rows, ctl.Resize.Cols)
			}
			continue
		}
		if readOnly {
			continue // view-only: ignore keystrokes
		}
		if _, err := pty.Write(data); err != nil {
			return
		}
	}
}
