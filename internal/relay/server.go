// SPDX-License-Identifier: AGPL-3.0-or-later

package relay

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
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

// Server serves the browser terminal: a token page and a WebSocket bridged to the
// target node's SSH PTY. It holds no keys itself — each session carries its own
// scoped key, resolved from the Store by the presented token.
type Server struct {
	store *Store
	dial  Dialer
	now   func() time.Time
}

// NewServer builds a relay server over a session store and a target dialer.
func NewServer(store *Store, dial Dialer) *Server {
	return &Server{store: store, dial: dial, now: func() time.Time { return time.Now().UTC() }}
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
		http.NotFound(w, r)
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
		http.NotFound(w, r)
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
	srv.pump(ctx, c, pty)
	c.Close(websocket.StatusNormalClosure, "session closed")
}

// pump bridges a WebSocket and a PTY: PTY output -> binary WS frames; browser input
// (binary) -> PTY stdin; browser control (text JSON {"resize":{cols,rows}}) -> resize.
func (srv *Server) pump(ctx context.Context, c *websocket.Conn, pty PTYConn) {
	c.SetReadLimit(1 << 20)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
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
		if _, err := pty.Write(data); err != nil {
			return
		}
	}
}
