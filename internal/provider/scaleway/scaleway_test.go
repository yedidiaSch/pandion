package scaleway

import (
	"net"
	"os"
	"testing"
	"time"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// compile-time: Scaleway satisfies the core seam plus the Pricer. It does NOT
// implement AuxReaper — the login key rides the cloud-init user-data, and volumes
// are cleaned up inline by DestroyServer, so there is no auxiliary resource to reap.
var (
	_ provider.Provider = (*Scaleway)(nil)
	_ provider.Pricer   = (*Scaleway)(nil)
)

func TestSelectTypes_CheapestFirst_FiltersSpecGPUEOS(t *testing.T) {
	types := []typeInfo{
		{Name: "PLAY2-PICO", NCPUs: 1, RAMMB: 2048, HourlyEUR: 0.006},
		{Name: "PLAY2-NANO", NCPUs: 2, RAMMB: 4096, HourlyEUR: 0.012},
		{Name: "PRO2-XXS", NCPUs: 2, RAMMB: 8192, HourlyEUR: 0.028},
		{Name: "DEV1-S-EOS", NCPUs: 2, RAMMB: 4096, HourlyEUR: 0.001, EOS: true},
		{Name: "RENDER-S", NCPUs: 10, RAMMB: 45056, HourlyEUR: 1.24, GPU: true},
	}

	// spec: >=2 vcpu / >=4GB. Excludes the 1-vcpu, the GPU and the EOS bargain;
	// cheapest-first ⇒ PLAY2-NANO before PRO2-XXS.
	got := selectTypes(types, 2, 4096)
	if len(got) != 2 || got[0].Name != "PLAY2-NANO" || got[1].Name != "PRO2-XXS" {
		t.Fatalf("unexpected selection: %+v", names(got))
	}
}

func TestOrderZones_PreferredFirst(t *testing.T) {
	got := orderZones([]string{"fr-par-1", "nl-ams-1", "pl-waw-1"}, []string{"nl-ams-1", "de-fra-1"})
	want := []string{"nl-ams-1", "fr-par-1", "pl-waw-1"} // nl first, de-fra ignored (absent)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("orderZones = %v, want %v", got, want)
	}
}

func TestTagRoundTrip(t *testing.T) {
	for _, id := range []string{"pipeline", "e2e-lscost", "Build-01"} {
		tag := clusterTag(id)
		if got := recoverClusterID([]string{tagAll, tag}); got != sanitize(id) {
			t.Fatalf("round-trip %q: tag %q -> %q, want %q", id, tag, got, sanitize(id))
		}
	}
	if got := recoverClusterID([]string{"pandion"}); got != "" {
		t.Fatalf("no cluster tag should recover to empty, got %q", got)
	}
}

func TestInstanceName_SanitizedAndBounded(t *testing.T) {
	if n := instanceName("pipeline", "broker"); n != "pandion-pipeline-broker" {
		t.Fatalf("name = %q", n)
	}
	long := instanceName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if len(long) > 63 {
		t.Fatalf("name not bounded to 63: %d", len(long))
	}
}

func TestToTypeInfo_ConvertsRAMAndGPU(t *testing.T) {
	gpu := uint64(1)
	st := &instance.ServerType{Ncpus: 4, RAM: 8 * 1024 * 1024 * 1024, HourlyPrice: 0.05, Gpu: &gpu, EndOfService: false}
	ti := toTypeInfo("RENDER-S", st)
	if ti.RAMMB != 8192 {
		t.Fatalf("RAM MB = %d, want 8192", ti.RAMMB)
	}
	if !ti.GPU {
		t.Fatal("expected GPU=true when Gpu>0")
	}
}

func TestToServer_ParsesFields(t *testing.T) {
	created := time.Now().Add(-90 * time.Minute).UTC()
	srv := &instance.Server{
		ID:             "uuid-1",
		Name:           "pandion-pipeline-broker",
		CommercialType: "PLAY2-NANO",
		Zone:           scw.Zone("nl-ams-1"),
		Tags:           []string{tagAll, clusterTag("pipeline")},
		CreationDate:   &created,
		PublicIP:       &instance.ServerIP{Address: net.ParseIP("203.0.113.7"), Family: instance.ServerIPIPFamilyInet},
	}
	s := toServer(srv)
	if s.ID != "uuid-1" || s.Type != "PLAY2-NANO" || s.Region != "nl-ams-1" || s.IP != "203.0.113.7" {
		t.Fatalf("toServer mapped wrong: %+v", s)
	}
	if s.ClusterID != sanitize("pipeline") {
		t.Fatalf("cluster id not recovered: %q", s.ClusterID)
	}
	if s.Created.IsZero() || time.Since(s.Created) < time.Hour {
		t.Fatalf("created not parsed: %v", s.Created)
	}
}

func TestBlockVolumeIDs_OnlyBlock(t *testing.T) {
	srv := &instance.Server{Volumes: map[string]*instance.VolumeServer{
		"0": {ID: "vol-local", VolumeType: instance.VolumeServerVolumeTypeLSSD},
		"1": {ID: "vol-block", VolumeType: instance.VolumeServerVolumeTypeSbsVolume},
	}}
	got := blockVolumeIDs(srv)
	if len(got) != 1 || got[0] != "vol-block" {
		t.Fatalf("blockVolumeIDs = %v, want [vol-block]", got)
	}
}

func TestConfigFileExists_HonorsSCWConfigPath(t *testing.T) {
	// point SCW_CONFIG_PATH at a real then a missing file.
	f := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(f, []byte("access_key: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCW_CONFIG_PATH", f)
	if !ConfigFileExists() {
		t.Fatal("expected true when SCW_CONFIG_PATH points at an existing file")
	}
	t.Setenv("SCW_CONFIG_PATH", f+".nope")
	if ConfigFileExists() {
		t.Fatal("expected false when SCW_CONFIG_PATH points at a missing file")
	}
}

func TestIsNotFoundErr(t *testing.T) {
	if !isNotFoundErr(&scw.ResponseError{StatusCode: 404, Message: "not found"}) {
		t.Fatal("404 should be not-found")
	}
	if isNotFoundErr(&scw.ResponseError{StatusCode: 429, Message: "rate limited"}) {
		t.Fatal("429 is not not-found")
	}
}

func names(ts []typeInfo) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
