package sshkeys

import (
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestGenerate_RoundTripAndFormat(t *testing.T) {
	kp, err := Generate("envcore-host")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// OpenSSH private-key PEM (this is the format cloud-init ssh_keys requires).
	if !strings.HasPrefix(kp.PrivatePEM, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("private key is not OpenSSH PEM:\n%s", kp.PrivatePEM)
	}
	// Parsing the private key back must yield the same public key (round trip).
	signer, err := gossh.ParsePrivateKey([]byte(kp.PrivatePEM))
	if err != nil {
		t.Fatalf("parse private: %v", err)
	}
	if string(signer.PublicKey().Marshal()) != string(kp.Public.Marshal()) {
		t.Fatal("parsed public key does not match generated public key")
	}

	if !strings.HasPrefix(kp.PublicAuthorized, "ssh-ed25519 ") {
		t.Fatalf("authorized line malformed: %q", kp.PublicAuthorized)
	}
	if !strings.HasSuffix(kp.PublicAuthorized, " envcore-host") {
		t.Fatalf("comment missing from authorized line: %q", kp.PublicAuthorized)
	}
	if !strings.HasPrefix(kp.Fingerprint(), "SHA256:") {
		t.Fatalf("fingerprint malformed: %q", kp.Fingerprint())
	}
}

func TestGenerate_KeysAreUnique(t *testing.T) {
	a, _ := Generate("")
	b, _ := Generate("")
	if a.PublicAuthorized == b.PublicAuthorized {
		t.Fatal("two generated keys must differ")
	}
}
