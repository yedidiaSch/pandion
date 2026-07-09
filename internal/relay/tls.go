// SPDX-License-Identifier: AGPL-3.0-or-later

package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// EnsureSelfSigned writes a self-signed TLS cert+key to certPath/keyPath if they do
// not already exist, valid for the given hosts (IPs or DNS names), and returns the
// cert's SHA-256 fingerprint. v1 relays use self-signed TLS; the fingerprint lets
// the operator/participant verify the cert out of band (a `--domain` Let's Encrypt
// path is Phase 2). Idempotent: an existing cert is left untouched and its
// fingerprint returned.
func EnsureSelfSigned(certPath, keyPath string, hosts []string) (string, error) {
	if fp, err := certFingerprint(certPath); err == nil {
		return fp, nil // already present
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pandion-relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(825 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(strings.TrimSpace(h)); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h = strings.TrimSpace(h); h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", err
	}
	return fingerprint(der), nil
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := hex.EncodeToString(sum[:])
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(strings.ToUpper(h[i : i+2]))
	}
	return b.String()
}

func certFingerprint(certPath string) (string, error) {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	return PEMFingerprint(pemBytes)
}

// PEMFingerprint returns the colon-separated uppercased SHA-256 fingerprint of the
// first certificate in a PEM blob (e.g. read back from the relay node to show/pin).
func PEMFingerprint(pemBytes []byte) (string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("no PEM certificate found")
	}
	return fingerprint(block.Bytes), nil
}
