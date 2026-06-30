// Package provider defines the single seam between EnvCore's orchestration and
// any cloud backend. v1 implements `hetzner`; `mock` backs the tests.
//
// M0 deliberately exposes only the minimal lifecycle surface (create / destroy /
// list-by-tag) needed to prove the state machine + reconcile loop. The networking,
// firewall, volume and overlay methods from the architecture doc land in M1–M2.
//
// Note (spike S1 finding F2): there is intentionally NO HostKeyFingerprint method.
// EnvCore generates the host key locally and injects it via cloud-init ssh_keys,
// so it already knows the fingerprint to pin — no retrieval is needed.
package provider

import "context"

// ServerSpec describes a server to create. Type is selected by SPEC (cores/RAM)
// plus a region preference, never by a hardcoded type name — spike S1 finding F3:
// Hetzner type names rotate (cpx11 retired) and availability is sparse per region.
type ServerSpec struct {
	Name       string
	ClusterID  string
	MinCores   int
	MinRAMGB   int
	RegionPref []string
	Image      string
	UserData   string // cloud-init user-data (incl. injected ssh_keys host key)
}

// Server is a provisioned host.
type Server struct {
	ID        string
	Name      string
	ClusterID string
	Type      string
	Region    string
	IP        string
}

// Provider is the cloud backend contract.
type Provider interface {
	// CreateServer provisions a tagged server from spec.
	CreateServer(ctx context.Context, spec ServerSpec) (Server, error)
	// DestroyServer removes a server by id. MUST be idempotent: destroying an
	// already-absent server is success (enables safe retry + re-run).
	DestroyServer(ctx context.Context, id string) error
	// ListByTag returns all servers for a cluster. This is the reconciliation
	// SOURCE OF TRUTH (C4) — used even when local state is lost.
	ListByTag(ctx context.Context, clusterID string) ([]Server, error)
	// Name identifies the backend (e.g. "hetzner", "mock").
	Name() string
}
