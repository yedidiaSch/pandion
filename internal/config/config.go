// Package config loads and validates cluster.yaml against the published JSON
// Schema (draft 2020-12). Validation is pure and offline: it is the M3 foundation
// that every orchestration slice builds on, and backs `pandion validate`.
package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schema.json
var schemaJSON []byte

const schemaURL = "https://pandion.dev/schema/cluster.schema.json"

// Cluster is the typed view of cluster.yaml (a subset of the schema — the fields
// the orchestrator consumes; the schema is the full source of truth for validity).
type Cluster struct {
	APIVersion string     `yaml:"apiVersion"`
	Name       string     `yaml:"name"`
	Provider   Provider   `yaml:"provider"`
	Defaults   NodeCommon `yaml:"defaults"`
	Nodes      []Node     `yaml:"nodes"`
	Lifecycle  Lifecycle  `yaml:"lifecycle"`
}

// Provider selects and configures the cloud backend.
type Provider struct {
	Name       string `yaml:"name"`
	Credential string `yaml:"credential"`
	Region     string `yaml:"region"`
}

// NodeCommon holds fields valid at both defaults and per-node level.
type NodeCommon struct {
	Target         string     `yaml:"target"`
	Engine         string     `yaml:"engine"`
	Size           string     `yaml:"size"`
	Image          string     `yaml:"image"`
	ContainerImage string     `yaml:"container_image"` // for engine=docker
	TTL            string     `yaml:"ttl"`
	Toolchain      *Toolchain `yaml:"toolchain"`
	Sync           *Sync      `yaml:"sync"`
	Sec            *Security  `yaml:"security"`
	// Setup is a list of shell commands run on the node (as root) in the
	// egress-open build window — for software apt can't install: pip/npm/cargo,
	// a vendor repo, a binary fetched by curl, etc. Defaults' setup runs first,
	// then the node's own (additive).
	Setup []string `yaml:"setup"`
}

// Toolchain is the extra apt packages (libraries/tools) installed on a node. By
// default these are ADDED to Pandion's built-in C++ toolchain; set NoDefault to
// install ONLY this list (a minimal node).
type Toolchain struct {
	Packages  []string `yaml:"packages"`
	NoDefault bool     `yaml:"no_default"`
}

// Node is one host in the topology.
type Node struct {
	NodeCommon      `yaml:",inline"`
	Name            string   `yaml:"name"`
	Role            string   `yaml:"role"`
	Run             string   `yaml:"run"`
	IPCPorts        []string `yaml:"ipc_ports"`
	PrivilegedPorts []string `yaml:"privileged_ports"`
	NeedsCaps       []string `yaml:"needs_caps"`
	EgressAllow     []string `yaml:"egress_allow"`
}

// Sync configures workspace synchronization.
type Sync struct {
	Mode       string `yaml:"mode"`
	Path       string `yaml:"path"`
	RemotePath string `yaml:"remote_path"`
	Build      string `yaml:"build"`
}

// L2 overlay defaults.
const (
	DefaultL2Subnet = "192.168.66.0/24"
	DefaultL2VNI    = 100
)

// L2Overlay is the parsed `security.overlay: l2` config — an encrypted Layer-2
// (VXLAN-over-WireGuard) segment. Profile "safe" is a spoof-resistant hardened
// LAN; "lab" is a deliberately attackable cyber-range (Phase 2).
type L2Overlay struct {
	Profile string // "safe" (default) | "lab"
	Subnet  string // default 192.168.66.0/24
	VNI     int    // default 100
}

// Security holds the per-node hardening overrides.
type Security struct {
	Overlay               any      `yaml:"overlay"`
	EgressAllow           []string `yaml:"egress_allow"`
	OpenEgressDuringBuild *bool    `yaml:"open_egress_during_build"`
	RunAs                 string   `yaml:"run_as"`
	EncryptVolumes        *bool    `yaml:"encrypt_volumes"`
	BlockMetadataService  *bool    `yaml:"block_metadata_service"`
	AuditLog              *bool    `yaml:"audit_log"`
}

// Lifecycle mirrors the session/teardown defaults.
type Lifecycle struct {
	DestroyOnExit   *bool `yaml:"destroy_on_exit"`
	ConfirmTeardown *bool `yaml:"confirm_teardown"`
	KeepOnFailure   *bool `yaml:"keep_on_failure"`
}

// Effective is a node's settings after merging cluster defaults with its own
// overrides (node wins). It's what the orchestrator should consume.
type Effective struct {
	Size  string
	Image string
	// Packages are the node's EXTRA apt packages (declared libraries/tools). They
	// are added to the built-in toolchain unless NoDefaultToolchain is set.
	Packages           []string
	NoDefaultToolchain bool
	// Setup are shell commands run on the node (as root) in the build window,
	// after packages and before the workspace build — for non-apt software.
	Setup       []string
	Region      string
	RunUser     string   // security.run_as; empty means "use the default"
	TTLRaw      string   // ttl string ("60m" | "false" | ""); "" means "use the default"
	EgressAllow []string // union of node + security + defaults egress allowlists
	// Security defaults are ON (secure by default); a cluster.yaml `security:`
	// false explicitly opts out.
	BlockMetadata bool // block the cloud metadata endpoint (S-F)
	AuditLog      bool // install auditd baseline logging (S-F)
	// EncryptVolumes defaults OFF (opt-in) — LUKS makes the volume unrecoverable
	// after reboot, so it's only enabled when explicitly requested.
	EncryptVolumes bool
	// Engine is "native" (default) or "docker"; ContainerImage is the image for
	// engine=docker (default "ubuntu:24.04").
	Engine         string
	ContainerImage string
	// L2, if set, requests an encrypted Layer-2 overlay (security.overlay: l2).
	// nil means the default L3-only WireGuard overlay.
	L2 *L2Overlay
}

// Effective resolves a node's effective settings against the cluster defaults.
func (c *Cluster) Effective(n Node) Effective {
	pick := func(node, def string) string {
		if node != "" {
			return node
		}
		return def
	}
	e := Effective{
		Size:           pick(n.Size, c.Defaults.Size),
		Image:          pick(n.Image, c.Defaults.Image),
		Region:         c.Provider.Region,
		TTLRaw:         pick(n.TTL, c.Defaults.TTL),
		EgressAllow:    c.egressAllow(n),
		BlockMetadata:  c.secBool(n, func(s *Security) *bool { return s.BlockMetadataService }, true),
		AuditLog:       c.secBool(n, func(s *Security) *bool { return s.AuditLog }, true),
		EncryptVolumes: c.secBool(n, func(s *Security) *bool { return s.EncryptVolumes }, false),
		Engine:         pick(n.Engine, c.Defaults.Engine),
		ContainerImage: pick(n.ContainerImage, c.Defaults.ContainerImage),
		L2:             c.resolveL2(n),
	}
	if e.Engine == "" {
		e.Engine = "native" // unset ⇒ native (host), preserving existing behavior
	}
	if e.Engine == "docker" && e.ContainerImage == "" {
		e.ContainerImage = "ubuntu:24.04"
	}
	// Toolchain resolution: a node's toolchain wins over defaults. Packages are the
	// user's EXTRA libraries/tools (added to the built-in toolchain unless NoDefault).
	switch {
	case n.Toolchain != nil:
		e.Packages = n.Toolchain.Packages
		e.NoDefaultToolchain = n.Toolchain.NoDefault
	case c.Defaults.Toolchain != nil:
		e.Packages = c.Defaults.Toolchain.Packages
		e.NoDefaultToolchain = c.Defaults.Toolchain.NoDefault
	}
	switch {
	case n.Sec != nil && n.Sec.RunAs != "":
		e.RunUser = n.Sec.RunAs
	case c.Defaults.Sec != nil:
		e.RunUser = c.Defaults.Sec.RunAs
	}
	// Setup commands are additive: cluster defaults first, then the node's own.
	e.Setup = append(append([]string{}, c.Defaults.Setup...), n.Setup...)
	return e
}

// egressAllow unions the node-level and security-level allowlists with the
// cluster defaults' security allowlist (egress rules are additive), deduped.
func (c *Cluster) egressAllow(n Node) []string {
	seen := map[string]bool{}
	var out []string
	add := func(xs []string) {
		for _, x := range xs {
			if x != "" && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	add(n.EgressAllow)
	if n.Sec != nil {
		add(n.Sec.EgressAllow)
	}
	if c.Defaults.Sec != nil {
		add(c.Defaults.Sec.EgressAllow)
	}
	return out
}

// secBool resolves a *bool security toggle: node override, else defaults, else
// the secure-by-default fallback.
func (c *Cluster) secBool(n Node, get func(*Security) *bool, fallback bool) bool {
	if n.Sec != nil {
		if v := get(n.Sec); v != nil {
			return *v
		}
	}
	if c.Defaults.Sec != nil {
		if v := get(c.Defaults.Sec); v != nil {
			return *v
		}
	}
	return fallback
}

// resolveL2 resolves the L2 overlay for a node: node override wins, else the
// cluster defaults. nil means the default L3-only overlay.
func (c *Cluster) resolveL2(n Node) *L2Overlay {
	if n.Sec != nil && n.Sec.Overlay != nil {
		if o := parseOverlay(n.Sec.Overlay); o != nil {
			return o
		}
	}
	if c.Defaults.Sec != nil && c.Defaults.Sec.Overlay != nil {
		if o := parseOverlay(c.Defaults.Sec.Overlay); o != nil {
			return o
		}
	}
	return nil
}

// parseOverlay interprets a security.overlay value. It accepts the string form
// "l2" (⇒ profile safe) or the object form { l2: { profile, subnet, vni } }.
// Anything else (nil, "auto", false, an unrelated map) means "no L2 overlay".
// The JSON schema is the source of truth for validity; this is lenient by design.
func parseOverlay(v any) *L2Overlay {
	def := func() *L2Overlay { return &L2Overlay{Profile: "safe", Subnet: DefaultL2Subnet, VNI: DefaultL2VNI} }
	switch t := v.(type) {
	case string:
		if t == "l2" {
			return def()
		}
	case map[string]any:
		m, ok := t["l2"].(map[string]any)
		if !ok {
			if _, present := t["l2"]; !present {
				return nil
			}
			return def() // `l2:` with no sub-fields
		}
		o := def()
		if p, ok := m["profile"].(string); ok && p != "" {
			o.Profile = p
		}
		if s, ok := m["subnet"].(string); ok && s != "" {
			o.Subnet = s
		}
		switch vni := m["vni"].(type) {
		case int:
			o.VNI = vni
		case int64:
			o.VNI = int(vni)
		case float64:
			o.VNI = int(vni)
		}
		return o
	}
	return nil
}

// Validate checks raw YAML bytes against the schema without unmarshalling into
// typed structs. Returns nil if valid.
func Validate(data []byte) error {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}
	// normalize to JSON-canonical types (string keys, float64 numbers) so the
	// schema validator sees exactly what it expects.
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("normalize: %w", err)
	}

	var schemaDoc any
	if err := json.Unmarshal(schemaJSON, &schemaDoc); err != nil {
		return fmt.Errorf("load schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, schemaDoc); err != nil {
		return fmt.Errorf("add schema: %w", err)
	}
	sch, err := c.Compile(schemaURL)
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	if err := sch.Validate(raw); err != nil {
		return err // jsonschema.ValidationError has a detailed message
	}
	return nil
}

// Load reads, validates, and unmarshals cluster.yaml into a typed Cluster.
func Load(path string) (*Cluster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := Validate(data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var c Cluster
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}
