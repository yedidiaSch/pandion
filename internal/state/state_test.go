package state

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	c := &Cluster{
		ID: "pipeline", Provider: "hetzner",
		Nodes: []Node{
			{Name: "broker", ServerID: "111", IP: "1.2.3.4", Phase: Running},
			{Name: "worker", Phase: Planned},
		},
	}
	if err := s.Save(c); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Save stamps Updated
	if c.Updated.IsZero() {
		t.Error("Save should stamp Updated")
	}

	got, err := s.Load("pipeline")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ID != "pipeline" || got.Provider != "hetzner" || len(got.Nodes) != 2 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Nodes[0].ServerID != "111" || got.Nodes[0].Phase != Running || got.Nodes[1].Phase != Planned {
		t.Fatalf("node fields lost: %+v", got.Nodes)
	}
}

// Save is atomic (write-temp-then-rename): no leftover .tmp, and re-saving
// overwrites cleanly (resumability — journaling each transition).
func TestSaveAtomicAndOverwrite(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	c := &Cluster{ID: "x", Nodes: []Node{{Name: "n", Phase: Provisioning}}}
	if err := s.Save(c); err != nil {
		t.Fatalf("save: %v", err)
	}
	// advance the phase and re-journal
	c.Nodes[0].Phase = Running
	first := c.Updated
	time.Sleep(2 * time.Millisecond)
	if err := s.Save(c); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if !c.Updated.After(first) {
		t.Error("Updated should advance on re-save")
	}
	got, _ := s.Load("x")
	if got.Nodes[0].Phase != Running {
		t.Fatalf("overwrite lost the new phase: %+v", got.Nodes)
	}
	// no .tmp left behind
	if _, err := os.Stat(filepath.Join(dir, "x.json.tmp")); !os.IsNotExist(err) {
		t.Error("atomic save left a .tmp file")
	}
}

func TestLoadMissingErrors(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if _, err := s.Load("nope"); err == nil {
		t.Error("loading a missing cluster should error")
	}
}

// Close removes the record; closing an absent record is success (idempotent
// teardown — no error when the cluster is already gone).
func TestCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	s.Save(&Cluster{ID: "gone"})
	if err := s.Close("gone"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.json")); !os.IsNotExist(err) {
		t.Error("Close should remove the record file")
	}
	if err := s.Close("gone"); err != nil {
		t.Errorf("closing an absent record must be success, got %v", err)
	}
}

// NewStore creates the directory with 0700.
func TestNewStorePerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := NewStore(dir); err != nil {
		t.Fatalf("new store: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" { // POSIX perms don't apply on Windows
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("state dir perms = %o, want 0700", perm)
		}
	}
}
