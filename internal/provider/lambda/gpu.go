// SPDX-License-Identifier: AGPL-3.0-or-later

package lambda

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// GPUOfferings implements provider.GPUProvider: the priced GPU catalog, built
// from GET /instance-types. Only types with a GPU and current capacity are
// returned, cheapest-first. Lambda has no separate pricing endpoint — the price
// is on the instance type itself.
func (l *Lambda) GPUOfferings(ctx context.Context) ([]provider.GPUOffering, error) {
	types, err := l.instanceTypes(ctx)
	if err != nil {
		return nil, err
	}
	var out []provider.GPUOffering
	for _, t := range types {
		if t.itype.Specs.GPUs <= 0 {
			continue
		}
		gpu, ok := parseGPU(t.itype.Name, t.itype.Description)
		if !ok {
			continue
		}
		out = append(out, provider.GPUOffering{
			ServerType: t.itype.Name,
			GPU:        gpu,
			Regions:    t.regions,
			Hourly:     centsToMoney(t.itype.PriceCentsPerHr),
			Image:      "", // Lambda Stack is inherent; no image selection
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hourly.Amount != out[j].Hourly.Amount {
			return out[i].Hourly.Amount < out[j].Hourly.Amount
		}
		return out[i].ServerType < out[j].ServerType
	})
	return out, nil
}

// ResolveGPUType implements provider.GPUProvider: the cheapest offering that
// satisfies req AND has capacity in an acceptable region. Deterministic
// (cheapest-first) so --dry-run and up agree.
func (l *Lambda) ResolveGPUType(ctx context.Context, req provider.GPUReq, regionPref []string) (string, string, error) {
	offs, err := l.GPUOfferings(ctx)
	if err != nil {
		return "", "", err
	}
	for _, o := range offs {
		if !offeringSatisfies(o, req) {
			continue
		}
		if r := pickRegion(o.Regions, regionPref); r != "" {
			return o.ServerType, r, nil
		}
	}
	return "", "", fmt.Errorf("lambda: no GPU offering with capacity matches model=%q count=%d minVRAM=%d (try `pandion list-gpus --provider lambda`)",
		req.Model, req.Count, req.MinVRAM)
}

// offeringSatisfies reports whether an offering meets a GPU request. Count is a
// per-node GPU count, matched against the SKU's GPU count.
func offeringSatisfies(o provider.GPUOffering, req provider.GPUReq) bool {
	if req.Model != "" && !strings.EqualFold(req.Model, o.GPU.Model) {
		return false
	}
	if req.MinVRAM > 0 && o.GPU.VRAM < req.MinVRAM {
		return false
	}
	if req.Count > 0 && o.GPU.Count != req.Count {
		return false
	}
	return true
}

// offeringFor returns the offering for an exact type name (for an explicit --size).
func (l *Lambda) offeringFor(ctx context.Context, typeName string) (provider.GPUOffering, error) {
	offs, err := l.GPUOfferings(ctx)
	if err != nil {
		return provider.GPUOffering{}, err
	}
	for _, o := range offs {
		if o.ServerType == typeName {
			return o, nil
		}
	}
	return provider.GPUOffering{}, fmt.Errorf("lambda: unknown or unavailable instance type %q", typeName)
}

// HourlyPrice implements provider.Pricer: the gross hourly price for a type. The
// region is irrelevant on Lambda (uniform pricing). Unknown ⇒ zero Money, nil err.
func (l *Lambda) HourlyPrice(ctx context.Context, serverType, _ string) (provider.Money, error) {
	types, err := l.instanceTypes(ctx)
	if err != nil {
		return provider.Money{}, err
	}
	for _, t := range types {
		if t.itype.Name == serverType {
			return centsToMoney(t.itype.PriceCentsPerHr), nil
		}
	}
	return provider.Money{}, nil // unknown, don't error a listing
}

// EstimateHourly implements provider.Pricer: price the type a spec WOULD launch,
// resolving a --gpu request the same way CreateServer does. Fails (nonzero-but-
// unknown returns zero Money, nil err) so callers degrade; a request that cannot
// resolve at all returns an error so the --max-cost guard fails closed.
func (l *Lambda) EstimateHourly(ctx context.Context, spec provider.ServerSpec) (provider.Money, error) {
	typeName := spec.Type
	if typeName == "" {
		if !spec.GPU.Wanted() {
			return provider.Money{}, fmt.Errorf("lambda: cannot price a non-GPU request (pass --gpu)")
		}
		var err error
		typeName, _, err = l.ResolveGPUType(ctx, spec.GPU, spec.RegionPref)
		if err != nil {
			return provider.Money{}, err
		}
	}
	return l.HourlyPrice(ctx, typeName, "")
}

// --- instance-types + GPU parsing ------------------------------------------

// offering is an internal join of an instance type and its available regions.
type offering struct {
	itype   itype
	regions []string
}

// instanceTypes fetches GET /instance-types and flattens the map into a slice,
// keeping only regions that currently have capacity.
func (l *Lambda) instanceTypes(ctx context.Context) ([]offering, error) {
	var r struct {
		Data map[string]struct {
			InstanceType itype    `json:"instance_type"`
			Regions      []region `json:"regions_with_capacity_available"`
		} `json:"data"`
	}
	if err := l.do(ctx, http.MethodGet, "/instance-types", nil, &r); err != nil {
		return nil, err
	}
	out := make([]offering, 0, len(r.Data))
	for _, v := range r.Data {
		regs := make([]string, 0, len(v.Regions))
		for _, rg := range v.Regions {
			regs = append(regs, rg.Name)
		}
		sort.Strings(regs)
		out = append(out, offering{itype: v.InstanceType, regions: regs})
	}
	return out, nil
}

var (
	// countRe pulls the GPU count from a type name like "gpu_8x_a100_sxm4".
	countRe = regexp.MustCompile(`(?i)_(\d+)x_`)
	// modelRe pulls a GPU model token from the type name.
	modelRe = regexp.MustCompile(`(?i)_\d+x_([a-z0-9]+)`)
	// vramRe pulls VRAM (GB) from a description like "1x A100 (40 GB SXM4)".
	vramRe = regexp.MustCompile(`(\d+)\s*GB`)
)

// parseGPU derives model/count/VRAM from a Lambda instance type name and
// description. Name carries count+model ("gpu_1x_a100_sxm4"); the human
// description carries VRAM ("1x A100 (40 GB SXM4)"). Returns ok=false if the
// name has no recognizable GPU token.
func parseGPU(name, desc string) (provider.GPUInfo, bool) {
	m := modelRe.FindStringSubmatch(name)
	if m == nil {
		return provider.GPUInfo{}, false
	}
	g := provider.GPUInfo{Model: strings.ToLower(m[1]), Count: 1}
	if c := countRe.FindStringSubmatch(name); c != nil {
		if n, err := strconv.Atoi(c[1]); err == nil && n > 0 {
			g.Count = n
		}
	}
	if v := vramRe.FindStringSubmatch(desc); v != nil {
		if gb, err := strconv.Atoi(v[1]); err == nil {
			g.VRAM = gb
		}
	}
	return g, true
}

func centsToMoney(cents int) provider.Money {
	if cents <= 0 {
		return provider.Money{}
	}
	return provider.Money{Amount: float64(cents) / 100.0, Currency: "USD"}
}

// --- SSH key management ------------------------------------------------------

type sshKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// ensureLoginKey makes sure the login public key is registered with Lambda (a
// launch needs a key NAME, not the material) and returns the name(s) to attach.
// It reuses an existing key with the same material, else registers a new one
// named by the key's fingerprint so re-runs are idempotent.
func (l *Lambda) ensureLoginKey(ctx context.Context, clusterID, pubKey string) ([]string, error) {
	pubKey = strings.TrimSpace(pubKey)
	if pubKey == "" {
		return nil, fmt.Errorf("lambda: no login public key to register")
	}
	var list struct {
		Data []sshKey `json:"data"`
	}
	if err := l.do(ctx, http.MethodGet, "/ssh-keys", nil, &list); err != nil {
		return nil, fmt.Errorf("list ssh keys: %w", err)
	}
	for _, k := range list.Data {
		if sameKey(k.PublicKey, pubKey) {
			return []string{k.Name}, nil
		}
	}
	name := fmt.Sprintf("pandion-%s-%s", sanitize(clusterID), keyFingerprint(pubKey))
	var created struct {
		Data sshKey `json:"data"`
	}
	err := l.do(ctx, http.MethodPost, "/ssh-keys", sshKey{Name: name, PublicKey: pubKey}, &created)
	if err != nil {
		return nil, fmt.Errorf("register ssh key: %w", err)
	}
	return []string{name}, nil
}

// sameKey compares two authorized-keys lines by their type+base64 material,
// ignoring any trailing comment.
func sameKey(a, b string) bool { return keyMaterial(a) == keyMaterial(b) && keyMaterial(a) != "" }

func keyMaterial(pub string) string {
	f := strings.Fields(strings.TrimSpace(pub))
	if len(f) >= 2 {
		return f[0] + " " + f[1]
	}
	return ""
}

func keyFingerprint(pub string) string {
	sum := sha256.Sum256([]byte(keyMaterial(pub)))
	return hex.EncodeToString(sum[:])[:12]
}
