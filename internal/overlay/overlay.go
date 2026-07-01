// Package overlay builds WireGuard configuration for the management overlay.
//
// Design (validated by spike S3): each node runs a wg0 interface; the operator's
// machine joins as a peer. Management/IPC can then ride the encrypted overlay
// while the public plane is minimized. For M2.2 (single node) the node peers with
// the operator; multi-node meshing lands in M3.
//
// Keys are generated in-process (Curve25519) so no external `wg` tool is needed
// and the logic is unit-testable offline.
package overlay

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// DefaultPort is the WireGuard UDP listen port.
const DefaultPort = 51820

// Keypair is a base64-encoded WireGuard key pair.
type Keypair struct {
	Private string
	Public  string
}

// GenerateKeypair creates a clamped Curve25519 WireGuard key pair.
func GenerateKeypair() (Keypair, error) {
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return Keypair{}, err
	}
	// WireGuard clamping.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return Keypair{}, err
	}
	return Keypair{
		Private: base64.StdEncoding.EncodeToString(priv[:]),
		Public:  base64.StdEncoding.EncodeToString(pub),
	}, nil
}

// NodeSpec renders a node's /etc/wireguard/wg0.conf.
type NodeSpec struct {
	PrivKey       string
	Address       string // e.g. 10.99.0.1/24
	ListenPort    int
	PeerPubKey    string // operator's public key
	PeerAllowedIP string // e.g. 10.99.0.2/32
}

// NodeConfig renders the node-side wg0.conf. The operator peer has no fixed
// endpoint (it roams / initiates), so the node just waits for it.
func NodeConfig(s NodeSpec) string {
	port := s.ListenPort
	if port == 0 {
		port = DefaultPort
	}
	var b strings.Builder
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "Address = %s\n", s.Address)
	fmt.Fprintf(&b, "ListenPort = %d\n", port)
	fmt.Fprintf(&b, "PrivateKey = %s\n", s.PrivKey)
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", s.PeerPubKey)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", s.PeerAllowedIP)
	return b.String()
}

// OperatorSpec renders the operator's local wg config.
type OperatorSpec struct {
	PrivKey       string
	Address       string // e.g. 10.99.0.2/32
	PeerPubKey    string // node's public key
	Endpoint      string // node public IP:port
	PeerAllowedIP string // e.g. 10.99.0.1/32
}

// OperatorConfig renders the operator-side config (brought up with wg-quick).
func OperatorConfig(s OperatorSpec) string {
	var b strings.Builder
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "Address = %s\n", s.Address)
	fmt.Fprintf(&b, "PrivateKey = %s\n", s.PrivKey)
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", s.PeerPubKey)
	fmt.Fprintf(&b, "Endpoint = %s\n", s.Endpoint)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", s.PeerAllowedIP)
	b.WriteString("PersistentKeepalive = 25\n")
	return b.String()
}
