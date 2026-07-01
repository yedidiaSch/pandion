package overlay

import (
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/curve25519"
)

func TestGenerateKeypair_ValidAndDerivable(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	priv, err := base64.StdEncoding.DecodeString(kp.Private)
	if err != nil || len(priv) != 32 {
		t.Fatalf("private key not 32 base64 bytes: %v (len=%d)", err, len(priv))
	}
	pub, err := base64.StdEncoding.DecodeString(kp.Public)
	if err != nil || len(pub) != 32 {
		t.Fatalf("public key not 32 base64 bytes: %v (len=%d)", err, len(pub))
	}
	// public must be derivable from private (WireGuard invariant)
	derived, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if base64.StdEncoding.EncodeToString(derived) != kp.Public {
		t.Fatal("public key does not match private key")
	}
	// clamping bits
	if priv[0]&7 != 0 || priv[31]&128 != 0 || priv[31]&64 == 0 {
		t.Fatal("private key not clamped per WireGuard")
	}
}

func TestGenerateKeypair_Unique(t *testing.T) {
	a, _ := GenerateKeypair()
	b, _ := GenerateKeypair()
	if a.Private == b.Private || a.Public == b.Public {
		t.Fatal("keys must be unique")
	}
}

func TestNodeConfig_HasInterfaceAndPeer(t *testing.T) {
	out := NodeConfig(NodeSpec{
		PrivKey: "NODEPRIV", Address: "10.99.0.1/24",
		PeerPubKey: "OPPUB", PeerAllowedIP: "10.99.0.2/32",
	})
	for _, want := range []string{
		"[Interface]", "Address = 10.99.0.1/24", "ListenPort = 51820",
		"PrivateKey = NODEPRIV", "[Peer]", "PublicKey = OPPUB", "AllowedIPs = 10.99.0.2/32",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("node config missing %q\n%s", want, out)
		}
	}
	// node has no fixed Endpoint for the roaming operator
	if strings.Contains(out, "Endpoint") {
		t.Error("node config must not pin an operator endpoint")
	}
}

func TestOperatorConfig_HasEndpointAndKeepalive(t *testing.T) {
	out := OperatorConfig(OperatorSpec{
		PrivKey: "OPPRIV", Address: "10.99.0.2/32",
		PeerPubKey: "NODEPUB", Endpoint: "1.2.3.4:51820", PeerAllowedIP: "10.99.0.1/32",
	})
	for _, want := range []string{
		"PrivateKey = OPPRIV", "Endpoint = 1.2.3.4:51820",
		"AllowedIPs = 10.99.0.1/32", "PersistentKeepalive = 25",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("operator config missing %q\n%s", want, out)
		}
	}
}
