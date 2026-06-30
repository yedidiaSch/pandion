// Package state is the journaled, on-disk cluster state store. It is a CACHE,
// not the source of truth — the provider (queried by tag) is authoritative (C4).
// Every transition is written atomically so a crash is always resumable.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Phase is a node lifecycle state (subset for M0; full machine in the arch doc).
type Phase string

const (
	Planned      Phase = "PLANNED"
	Provisioning Phase = "PROVISIONING"
	Running      Phase = "RUNNING"
	TearingDown  Phase = "TEARING_DOWN"
	Destroyed    Phase = "DESTROYED"
	Failed       Phase = "FAILED"
)

// Node is one host in a cluster.
type Node struct {
	Name     string `json:"name"`
	ServerID string `json:"server_id,omitempty"`
	Phase    Phase  `json:"phase"`
}

// Cluster is the journaled record for one EnvCore cluster.
type Cluster struct {
	ID       string    `json:"id"`
	Provider string    `json:"provider"`
	Nodes    []Node    `json:"nodes"`
	Updated  time.Time `json:"updated"`
}

// Store persists clusters under a directory (e.g. ~/.envcore/state).
type Store struct{ dir string }

// NewStore ensures the directory exists (0700) and returns a Store.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// Save journals the cluster state via a write-temp-then-rename (atomic).
func (s *Store) Save(c *Cluster) error {
	c.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(c.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(c.ID))
}

// Load reads a cluster record.
func (s *Store) Load(id string) (*Cluster, error) {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var c Cluster
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Close removes the record once a cluster is fully destroyed. Absent == success.
func (s *Store) Close(id string) error {
	err := os.Remove(s.path(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
