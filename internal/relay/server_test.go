// SPDX-License-Identifier: AGPL-3.0-or-later

package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakePTY stands in for an SSH PTY: it echoes what's written (upper-cased so the
// test can tell input from output) and records the last resize.
type fakePTY struct {
	mu         sync.Mutex
	buf        chan []byte
	closed     bool
	lastRows   int
	lastCols   int
	resizeSeen chan struct{}
}

func newFakePTY() *fakePTY {
	return &fakePTY{buf: make(chan []byte, 16), resizeSeen: make(chan struct{}, 4)}
}
func (f *fakePTY) Read(p []byte) (int, error) {
	b, ok := <-f.buf
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}
func (f *fakePTY) Write(p []byte) (int, error) {
	f.mu.Lock()
	c := f.closed
	f.mu.Unlock()
	if c {
		return 0, io.ErrClosedPipe
	}
	f.buf <- []byte(strings.ToUpper(string(p))) // echo, upper-cased
	return len(p), nil
}
func (f *fakePTY) Resize(rows, cols int) error {
	f.mu.Lock()
	f.lastRows, f.lastCols = rows, cols
	f.mu.Unlock()
	select {
	case f.resizeSeen <- struct{}{}:
	default:
	}
	return nil
}
func (f *fakePTY) Close() error {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.buf)
	}
	f.mu.Unlock()
	return nil
}

func testServer(t *testing.T) (*httptest.Server, *Store, *fakePTY) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pty := newFakePTY()
	srv := NewServer(store, func(s *Session) (PTYConn, error) { return pty, nil })
	return httptest.NewServer(srv.Handler()), store, pty
}

func liveSession(store *Store) (*Session, string) {
	tok, _ := NewToken()
	s := &Session{ID: "s1", Token: tok, ClusterID: "c1", Node: "victim", Target: "10.99.0.2",
		HostPub: "ssh-ed25519 AAAA host", User: "pandion-lab",
		SSHKeyPEM: "x", Expiry: time.Now().Add(time.Hour).UTC()}
	_ = store.Put(s)
	return s, tok
}

func TestServer_Page_ValidVs404(t *testing.T) {
	ts, store, _ := testServer(t)
	defer ts.Close()
	_, tok := liveSession(store)

	// valid token -> the terminal page, carrying the node + token.
	resp, err := http.Get(ts.URL + "/s/" + tok)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "xterm.js") || !strings.Contains(string(body), tok) {
		t.Fatalf("valid page: status=%d has-token=%v", resp.StatusCode, strings.Contains(string(body), tok))
	}
	// unknown token -> generic 404 (no oracle).
	resp2, _ := http.Get(ts.URL + "/s/PRLY1-nope")
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("unknown token should 404, got %d", resp2.StatusCode)
	}
}

func TestServer_WS_BridgeAndResize(t *testing.T) {
	ts, store, pty := testServer(t)
	defer ts.Close()
	_, tok := liveSession(store)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/" + tok
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")

	// keystrokes (binary) reach the PTY and its echo comes back.
	if err := c.Write(ctx, websocket.MessageBinary, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if typ != websocket.MessageBinary || string(data) != "HELLO" {
		t.Fatalf("bridge echo = %q (%v), want HELLO", data, typ)
	}

	// a resize control frame (text JSON) reaches PTY.Resize.
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"resize":{"cols":120,"rows":40}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pty.resizeSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("resize control frame not delivered to PTY")
	}
	pty.mu.Lock()
	rows, cols := pty.lastRows, pty.lastCols
	pty.mu.Unlock()
	if rows != 40 || cols != 120 {
		t.Fatalf("resize = %dx%d, want 40x120", rows, cols)
	}
}

func TestServer_WS_RejectsUnknownToken(t *testing.T) {
	ts, _, _ := testServer(t)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/PRLY1-nope", nil); err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("ws with an unknown token must be rejected")
	}
}

func TestServer_ReadOnly_DropsInput(t *testing.T) {
	ts, store, _ := testServer(t)
	defer ts.Close()
	tok, _ := NewToken()
	_ = store.Put(&Session{ID: "ro", Token: tok, Node: "n", Target: "1.2.3.4",
		HostPub: "ssh-ed25519 AAAA h", User: "u", SSHKeyPEM: "x",
		Expiry: time.Now().Add(time.Hour).UTC(), ReadOnly: true})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/"+tok, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")

	// keystrokes must be dropped — the echoing fake PTY should produce nothing back.
	_ = c.Write(ctx, websocket.MessageBinary, []byte("hello"))
	rctx, rcancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer rcancel()
	if _, data, rerr := c.Read(rctx); rerr == nil && strings.Contains(string(data), "HELLO") {
		t.Fatal("read-only session must not feed keystrokes to the PTY")
	}
}

func TestServer_RateLimitsUnknownTokens(t *testing.T) {
	ts, _, _ := testServer(t)
	defer ts.Close()
	got429 := false
	for i := 0; i < badTokenPerMin+5; i++ {
		resp, err := http.Get(ts.URL + "/s/PRLY1-nope")
		if err != nil {
			t.Fatal(err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatalf("expected a 429 after >%d unknown-token requests", badTokenPerMin)
	}
}

func TestServer_Recording_TeesOutput(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pty := newFakePTY()
	srv := NewServer(store, func(s *Session) (PTYConn, error) { return pty, nil })
	recDir := t.TempDir()
	srv.RecordDir = recDir
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tok, _ := NewToken()
	_ = store.Put(&Session{ID: "rec1", Token: tok, Node: "n", Target: "1.2.3.4",
		HostPub: "ssh-ed25519 AAAA h", User: "u", SSHKeyPEM: "x",
		Expiry: time.Now().Add(time.Hour).UTC(), Record: true})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/"+tok, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("hello")) // fake PTY echoes "HELLO"
	if _, data, rerr := c.Read(ctx); rerr != nil || !strings.Contains(string(data), "HELLO") {
		t.Fatalf("expected echo, got %q (%v)", data, rerr)
	}
	c.Close(websocket.StatusNormalClosure, "done")
	time.Sleep(300 * time.Millisecond) // let handleWS close the recording file

	entries, _ := os.ReadDir(recDir)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "rec1-") {
		t.Fatalf("expected one recording file rec1-*, got %v", entries)
	}
	b, _ := os.ReadFile(filepath.Join(recDir, entries[0].Name()))
	if !strings.Contains(string(b), "HELLO") {
		t.Fatalf("recording should contain the terminal output, got %q", b)
	}
}
