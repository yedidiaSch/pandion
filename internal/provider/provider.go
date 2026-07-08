// SPDX-License-Identifier: AGPL-3.0-or-later

// Package provider defines the single seam between Pandion's orchestration and
// any cloud backend. v1 implements `hetzner`; `mock` backs the tests.
//
// M0 deliberately exposes only the minimal lifecycle surface (create / destroy /
// list-by-tag) needed to prove the state machine + reconcile loop. The networking,
// firewall, volume and overlay methods from the architecture doc land in M1–M2.
//
// Note (spike S1 finding F2): there is intentionally NO HostKeyFingerprint method.
// Pandion generates the host key locally and injects it via cloud-init ssh_keys,
// so it already knows the fingerprint to pin — no retrieval is needed.
package provider

import (
	"context"
	"time"
)

// ServerSpec describes a server to create. Type is selected by SPEC (cores/RAM)
// plus a region preference, never by a hardcoded type name — spike S1 finding F3:
// Hetzner type names rotate (cpx11 retired) and availability is sparse per region.
type ServerSpec struct {
	Name       string
	ClusterID  string
	Type       string // exact provider type (e.g. "cpx21"); empty = discover by spec
	MinCores   int
	MinRAMGB   int
	RegionPref []string
	Image      string
	UserData   string // cloud-init user-data (incl. injected ssh_keys host key)
	// LoginPubKey, if set, is registered with the provider so it is installed
	// into root's authorized_keys reliably (validated path, spike S1). cloud-init
	// default-user semantics are NOT relied upon for the login key.
	LoginPubKey string
}

// Server is a provisioned host.
type Server struct {
	ID        string
	Name      string
	ClusterID string
	Type      string
	Region    string
	IP        string
	Created   time.Time // when the server was created (for `reap --older-than`)
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
	// ListAllTagged returns every server Pandion created (any cluster), for the
	// client-side orphan reaper (`pandion reap`) — the no-backend way to prevent
	// billing leaks when local state or the controlling laptop is gone (C4).
	ListAllTagged(ctx context.Context) ([]Server, error)
	// Name identifies the backend (e.g. "hetzner", "mock").
	Name() string
}

// AuxReaper is an optional Provider capability: clean up cluster-scoped auxiliary
// resources (e.g. registered SSH keys) that are not servers. The orchestrator
// calls it during Down after servers are destroyed, so nothing leaks (C4).
type AuxReaper interface {
	ReapAux(ctx context.Context, clusterID string) error
}

// ClusterFirewaller is an optional Provider capability: a provider-level ("cloud
// edge") firewall in front of the host nftables — defense-in-depth (M8). It is
// created during `up` and MUST be torn down by ReapAux so nothing leaks.
type ClusterFirewaller interface {
	// EnsureClusterFirewall create-or-confirms a firewall scoped to the cluster's
	// servers, allowing only SSH + the WireGuard port (wgPort) + ICMP inbound
	// (egress stays with the host). Idempotent.
	EnsureClusterFirewall(ctx context.Context, clusterID string, wgPort int) error
}

// Money is an amount in a provider's billing currency. A zero Money (Amount == 0
// or empty Currency) means "price unknown" — callers should render it as such
// rather than as free.
type Money struct {
	Amount   float64
	Currency string // ISO code, e.g. "EUR"
}

// Known reports whether the price is populated (vs. "unknown").
func (m Money) Known() bool { return m.Currency != "" && m.Amount > 0 }

// Pricer is an optional Provider capability: the gross hourly price of a server
// type in a region. It powers `ls`/`status` live cost (L1) and the `--max-cost`
// preflight (L2). Optional so the core Provider seam stays minimal — callers must
// degrade gracefully (omit cost) when a backend does not implement it.
type Pricer interface {
	// HourlyPrice returns the gross hourly price for serverType in region. An
	// unknown price is returned as a zero Money with a nil error (not an error),
	// so a missing entry never breaks a listing.
	HourlyPrice(ctx context.Context, serverType, region string) (Money, error)
	// EstimateHourly prices the server a spec WOULD provision, without creating
	// it — resolving an auto-selected type the same way CreateServer does — for
	// the `--max-cost` preflight. A zero Money (nil error) means "couldn't price".
	EstimateHourly(ctx context.Context, spec ServerSpec) (Money, error)
}
