// Package sshkeys generates ed25519 SSH key pairs for Pandion.
//
// Two pairs are used per node (spike S1):
//   - a HOST key, injected via cloud-init ssh_keys so Pandion already knows the
//     node's identity and can PIN it (defeating MITM/TOFU); and
//   - a LOGIN key, whose public half is added to authorized_keys.
package sshkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

// KeyPair holds both encodings Pandion needs.
type KeyPair struct {
	// PrivatePEM is the OpenSSH-format private key (for cloud-init / on-disk).
	PrivatePEM string
	// PublicAuthorized is the "ssh-ed25519 AAAA... comment" line.
	PublicAuthorized string
	// Signer authenticates as this key (login key).
	Signer gossh.Signer
	// Public is the parsed public key (host key, to pin).
	Public gossh.PublicKey
}

// Fingerprint returns the SHA256 fingerprint of the public key.
func (k *KeyPair) Fingerprint() string { return gossh.FingerprintSHA256(k.Public) }

// Generate creates a fresh ed25519 key pair with an optional comment.
func Generate(comment string) (*KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	block, err := gossh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	authorized := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		authorized += " " + comment
	}
	return &KeyPair{
		PrivatePEM:       string(pem.EncodeToMemory(block)),
		PublicAuthorized: authorized,
		Signer:           signer,
		Public:           sshPub,
	}, nil
}

// Save writes the private (0600) and public (0644) key files under dir/name.*.
func (k *KeyPair) Save(dir, name string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	priv := filepath.Join(dir, name)
	if err := os.WriteFile(priv, []byte(k.PrivatePEM), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	pub := filepath.Join(dir, name+".pub")
	if err := os.WriteFile(pub, []byte(k.PublicAuthorized+"\n"), 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}
