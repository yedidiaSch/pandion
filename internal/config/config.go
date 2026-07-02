// Package config loads and validates cluster.yaml against the published JSON
// Schema (draft 2020-12). Validation is pure and offline: it is the M3 foundation
// that every orchestration slice builds on, and backs `envcore validate`.
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

const schemaURL = "https://envcore.dev/schema/cluster.schema.json"

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
	Target    string     `yaml:"target"`
	Engine    string     `yaml:"engine"`
	Size      string     `yaml:"size"`
	Image     string     `yaml:"image"`
	TTL       string     `yaml:"ttl"`
	Toolchain *Toolchain `yaml:"toolchain"`
	Sync      *Sync      `yaml:"sync"`
	Sec       *Security  `yaml:"security"`
}

// Toolchain is the set of packages installed on a node.
type Toolchain struct {
	Packages []string `yaml:"packages"`
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
	Size     string
	Image    string
	Packages []string
	Region   string
	RunUser  string // security.run_as; empty means "use the default"
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
		Size:   pick(n.Size, c.Defaults.Size),
		Image:  pick(n.Image, c.Defaults.Image),
		Region: c.Provider.Region,
	}
	switch {
	case n.Toolchain != nil && len(n.Toolchain.Packages) > 0:
		e.Packages = n.Toolchain.Packages
	case c.Defaults.Toolchain != nil:
		e.Packages = c.Defaults.Toolchain.Packages
	}
	switch {
	case n.Sec != nil && n.Sec.RunAs != "":
		e.RunUser = n.Sec.RunAs
	case c.Defaults.Sec != nil:
		e.RunUser = c.Defaults.Sec.RunAs
	}
	return e
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
