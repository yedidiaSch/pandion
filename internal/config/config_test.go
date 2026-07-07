package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_ValidFixtures(t *testing.T) {
	for _, name := range []string{"valid_minimal.yaml", "valid_cluster.yaml", "valid_deploy_only.yaml"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := Validate(data); err != nil {
			t.Errorf("%s should be valid, got: %v", name, err)
		}
	}
}

func TestValidate_InvalidFixturesRejected(t *testing.T) {
	for _, name := range []string{
		"invalid_apiversion.yaml",
		"invalid_unknown_field.yaml",
		"invalid_badname.yaml",
	} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := Validate(data); err == nil {
			t.Errorf("%s should be REJECTED by the schema, but validated", name)
		}
	}
}

func TestLoad_TypedFields(t *testing.T) {
	c, err := Load(filepath.Join("testdata", "valid_cluster.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.APIVersion != "pandion/v1" || c.Name != "zmq-pipeline" {
		t.Fatalf("header not parsed: %+v", c)
	}
	if c.Provider.Name != "hetzner" || c.Provider.Region != "nbg1" {
		t.Fatalf("provider not parsed: %+v", c.Provider)
	}
	if c.Defaults.Engine != "native" || c.Defaults.Size != "cpx21" {
		t.Fatalf("defaults not parsed: %+v", c.Defaults)
	}
	if len(c.Nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(c.Nodes))
	}
	broker := c.Nodes[0]
	if broker.Name != "broker" || broker.Run != "./build/broker" {
		t.Fatalf("broker node not parsed: %+v", broker)
	}
	if len(broker.IPCPorts) != 2 || broker.IPCPorts[0] != "5557/tcp" {
		t.Fatalf("ipc_ports not parsed: %v", broker.IPCPorts)
	}
	if len(c.Nodes[1].NeedsCaps) != 1 || c.Nodes[1].NeedsCaps[0] != "NET_RAW" {
		t.Fatalf("needs_caps not parsed: %v", c.Nodes[1].NeedsCaps)
	}
}

func TestLoad_InvalidReturnsError(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "invalid_badname.yaml"))
	if err == nil {
		t.Fatal("expected load error for invalid cluster")
	}
	if !strings.Contains(err.Error(), "invalid_badname.yaml") {
		t.Errorf("error should name the file, got: %v", err)
	}
}

// A node may omit `run:` — that is a DEPLOY-ONLY node (provision + sync + build,
// started later with `pandion start`). It must validate and load with an empty Run.
func TestLoad_DeployOnlyNodeAllowed(t *testing.T) {
	c, err := Load(filepath.Join("testdata", "valid_deploy_only.yaml"))
	if err != nil {
		t.Fatalf("deploy-only cluster should load, got: %v", err)
	}
	if len(c.Nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(c.Nodes))
	}
	if c.Nodes[0].Name != "target" || c.Nodes[0].Run != "" {
		t.Fatalf("first node should be deploy-only (empty run): %+v", c.Nodes[0])
	}
	if c.Nodes[1].Run != "./exploit" {
		t.Fatalf("second node run not parsed: %+v", c.Nodes[1])
	}
}

// The published example in git/plan should stay valid against the embedded schema.
func TestValidate_MatchesPublishedExample(t *testing.T) {
	// best-effort: skip if the plan repo layout isn't present
	p := filepath.Join("..", "..", "..", "plan", "cluster.example.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("plan example not found (%v)", err)
	}
	if err := Validate(data); err != nil {
		t.Errorf("git/plan/cluster.example.yaml no longer matches the schema: %v", err)
	}
}

func TestEffective_DefaultsInheritanceAndOverride(t *testing.T) {
	c := &Cluster{
		Provider: Provider{Region: "nbg1"},
		Defaults: NodeCommon{Size: "cpx21", Image: "ubuntu-24.04",
			Toolchain: &Toolchain{Packages: []string{"nodejs", "npm"}}},
		Nodes: []Node{
			{Name: "web"}, // inherits all defaults
			{NodeCommon: NodeCommon{Size: "cpx31"}, Name: "worker"}, // overrides size
			{NodeCommon: NodeCommon{Toolchain: &Toolchain{Packages: []string{"postgresql"}}}, Name: "db"},
		},
	}
	web := c.Effective(c.Nodes[0])
	if web.Size != "cpx21" || web.Image != "ubuntu-24.04" || web.Region != "nbg1" {
		t.Fatalf("web inheritance wrong: %+v", web)
	}
	if len(web.Packages) != 2 || web.Packages[0] != "nodejs" {
		t.Fatalf("web packages inheritance wrong: %v", web.Packages)
	}
	if c.Effective(c.Nodes[1]).Size != "cpx31" {
		t.Fatalf("worker size override not applied")
	}
	if got := c.Effective(c.Nodes[2]).Packages; len(got) != 1 || got[0] != "postgresql" {
		t.Fatalf("db toolchain override not applied: %v", got)
	}
}

func TestEffective_EngineAndContainerImage(t *testing.T) {
	c := &Cluster{
		Defaults: NodeCommon{Engine: "docker"},
		Nodes: []Node{
			{Name: "a"}, // inherits engine=docker, default container image
			{Name: "b", NodeCommon: NodeCommon{Engine: "docker", ContainerImage: "alpine:3.20"}},
			{Name: "c", NodeCommon: NodeCommon{Engine: "native"}}, // override to native
		},
	}
	a := c.Effective(c.Nodes[0])
	if a.Engine != "docker" || a.ContainerImage != "ubuntu:24.04" {
		t.Fatalf("a: engine/image = %q/%q, want docker/ubuntu:24.04", a.Engine, a.ContainerImage)
	}
	if b := c.Effective(c.Nodes[1]); b.ContainerImage != "alpine:3.20" {
		t.Fatalf("b container image override not applied: %q", b.ContainerImage)
	}
	if cc := c.Effective(c.Nodes[2]); cc.Engine != "native" {
		t.Fatalf("c should override to native, got %q", cc.Engine)
	}

	// no engine anywhere ⇒ native (preserves existing behavior)
	none := &Cluster{Nodes: []Node{{Name: "x"}}}
	if e := none.Effective(none.Nodes[0]); e.Engine != "native" {
		t.Fatalf("unset engine should default to native, got %q", e.Engine)
	}
}

func TestEffective_EgressAllowUnionAndSecurityDefaults(t *testing.T) {
	no := false
	c := &Cluster{
		Defaults: NodeCommon{Sec: &Security{EgressAllow: []string{"10.0.0.0/8"}}},
		Nodes: []Node{
			// node-level + security-level egress, unioned with the default
			{Name: "a", EgressAllow: []string{"1.1.1.1/32"},
				NodeCommon: NodeCommon{Sec: &Security{EgressAllow: []string{"1.1.1.1/32", "8.8.8.8/32"}}}},
			// opts OUT of the metadata block + audit log
			{Name: "b", NodeCommon: NodeCommon{Sec: &Security{
				BlockMetadataService: &no, AuditLog: &no}}},
			{Name: "c"}, // pure defaults
		},
	}

	a := c.Effective(c.Nodes[0])
	// union deduped: 1.1.1.1/32, 8.8.8.8/32, 10.0.0.0/8 (default)
	want := map[string]bool{"1.1.1.1/32": true, "8.8.8.8/32": true, "10.0.0.0/8": true}
	if len(a.EgressAllow) != 3 {
		t.Fatalf("egress union wrong: %v", a.EgressAllow)
	}
	for _, e := range a.EgressAllow {
		if !want[e] {
			t.Fatalf("unexpected egress entry %q in %v", e, a.EgressAllow)
		}
	}

	// secure-by-default: on unless explicitly disabled
	if !a.BlockMetadata || !a.AuditLog {
		t.Fatalf("node a should default to on: %+v", a)
	}
	b := c.Effective(c.Nodes[1])
	if b.BlockMetadata || b.AuditLog {
		t.Fatalf("node b opted out but overrides ignored: %+v", b)
	}
	cc := c.Effective(c.Nodes[2])
	if !cc.BlockMetadata || !cc.AuditLog {
		t.Fatalf("node c (defaults) should be on: %+v", cc)
	}
}

func TestEffective_ToolchainAdditiveAndNoDefault(t *testing.T) {
	c := &Cluster{
		Defaults: NodeCommon{Toolchain: &Toolchain{Packages: []string{"libdefault-dev"}}},
		Nodes: []Node{
			{Name: "a"}, // inherits defaults' toolchain
			{Name: "b", NodeCommon: NodeCommon{Toolchain: &Toolchain{Packages: []string{"libzmq3-dev"}, NoDefault: true}}},
		},
	}
	// node a: inherits the defaults' extras, NoDefault stays false (additive).
	ea := c.Effective(c.Nodes[0])
	if len(ea.Packages) != 1 || ea.Packages[0] != "libdefault-dev" || ea.NoDefaultToolchain {
		t.Fatalf("node a should inherit defaults' extras additively: %+v", ea)
	}
	// node b: its own toolchain wins, NoDefault carried through (minimal node).
	eb := c.Effective(c.Nodes[1])
	if len(eb.Packages) != 1 || eb.Packages[0] != "libzmq3-dev" || !eb.NoDefaultToolchain {
		t.Fatalf("node b should override with no_default: %+v", eb)
	}
}

func TestEffective_SetupAdditiveDefaultsThenNode(t *testing.T) {
	c := &Cluster{
		Defaults: NodeCommon{Setup: []string{"pip install -r /req.txt"}},
		Nodes: []Node{
			{Name: "a", NodeCommon: NodeCommon{Setup: []string{"curl -sSL https://x | sh"}}},
			{Name: "b"}, // only the defaults' setup
		},
	}
	ea := c.Effective(c.Nodes[0])
	if len(ea.Setup) != 2 || ea.Setup[0] != "pip install -r /req.txt" || ea.Setup[1] != "curl -sSL https://x | sh" {
		t.Fatalf("node a setup should be defaults-then-node: %v", ea.Setup)
	}
	eb := c.Effective(c.Nodes[1])
	if len(eb.Setup) != 1 || eb.Setup[0] != "pip install -r /req.txt" {
		t.Fatalf("node b should inherit only defaults' setup: %v", eb.Setup)
	}
}
