// Package tlsutil manages the daemon's persistent self-signed TLS certificate and SHA-256 leaf-certificate
// fingerprint pinning. Both the direct TLS listener (--listen) and end-to-end relay streams use the same
// certificate so clients pin one fingerprint per host across every route.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
)

// ErrPinMismatch is returned when the peer's leaf certificate does not match the pinned SHA-256 fingerprint.
var ErrPinMismatch = errors.New("tls certificate fingerprint mismatch")

// FormatFingerprint returns the canonical "sha256:<64-lower-hex>" fingerprint of a DER-encoded certificate.
func FormatFingerprint(rawCert []byte) string {
	sum := sha256.Sum256(rawCert)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseFingerprint parses "sha256:<64-hex>" (case-insensitive) or a bare 64-hex SHA-256 digest.
func ParseFingerprint(s string) ([32]byte, error) {
	var out [32]byte
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(strings.ToLower(s), "sha256:"); ok {
		s = rest
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("invalid certificate fingerprint %q: want sha256:<64-hex>", s)
	}
	copy(out[:], b)
	return out, nil
}

// LoadOrCreate loads the daemon's TLS certificate and private key from certPath and keyPath, generating an
// ECDSA P-256 self-signed pair on first start. First-start creation stages both halves in a single atomic
// pending bundle so a crash between writing tls.key and tls.crt recovers on the next start. Once committed,
// if one file exists without the other or either fails to parse, it returns an error rather than rotating
// the certificate and invalidating paired clients' pins.
func LoadOrCreate(certPath, keyPath, commonName string) (*tls.Config, string, error) {
	pendingPath := certPath + ".pending"
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	_, pendErr := os.Stat(pendingPath)
	certExists := certErr == nil
	keyExists := keyErr == nil
	pendExists := pendErr == nil
	if certErr != nil && !errors.Is(certErr, os.ErrNotExist) {
		return nil, "", certErr
	}
	if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
		return nil, "", keyErr
	}
	if pendErr != nil && !errors.Is(pendErr, os.ErrNotExist) {
		return nil, "", pendErr
	}
	switch {
	case certExists && keyExists:
		if pendExists {
			_ = os.Remove(pendingPath)
		}
	case pendExists:
		certPEM, keyPEM, err := readPendingBundle(pendingPath)
		if err == nil {
			if err := commitKeypair(certPath, keyPath, pendingPath, certPEM, keyPEM); err != nil {
				return nil, "", err
			}
		} else if !certExists && !keyExists {
			_ = os.Remove(pendingPath)
			if err := generateSelfSigned(certPath, keyPath, pendingPath, commonName); err != nil {
				return nil, "", err
			}
		} else {
			return nil, "", fmt.Errorf("incomplete TLS keypair with corrupt staging bundle (%s): %w", pendingPath, err)
		}
	case certExists != keyExists:
		return nil, "", fmt.Errorf("incomplete TLS keypair (%s exists=%v, %s exists=%v): refusing to regenerate and break paired pins",
			certPath, certExists, keyPath, keyExists)
	default:
		if err := generateSelfSigned(certPath, keyPath, pendingPath, commonName); err != nil {
			return nil, "", err
		}
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, "", fmt.Errorf("loading TLS keypair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, "", errors.New("TLS certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, "", fmt.Errorf("parsing TLS leaf certificate: %w", err)
	}
	pair.Leaf = leaf
	cfg := &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	return cfg, FormatFingerprint(leaf.Raw), nil
}

func generateSelfSigned(certPath, keyPath, pendingPath, commonName string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return err
	}
	if commonName == "" {
		commonName = "agent-mesh"
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"agent-mesh.local", commonName},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	bundle := append(append([]byte(nil), certPEM...), keyPEM...)
	if err := fsutil.WriteAtomic(pendingPath, bundle, 0o600); err != nil {
		return err
	}
	return commitKeypair(certPath, keyPath, pendingPath, certPEM, keyPEM)
}

func commitKeypair(certPath, keyPath, pendingPath string, certPEM, keyPEM []byte) error {
	if err := fsutil.WriteAtomic(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(certPath, certPEM, 0o600); err != nil {
		return err
	}
	if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readPendingBundle(path string) (certPEM, keyPEM []byte, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	for len(data) > 0 {
		var blk *pem.Block
		blk, data = pem.Decode(data)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "CERTIFICATE":
			certPEM = pem.EncodeToMemory(blk)
		case "EC PRIVATE KEY", "PRIVATE KEY":
			keyPEM = pem.EncodeToMemory(blk)
		}
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, errors.New("pending bundle missing certificate or private key")
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// PinnedConfig returns a client tls.Config that verifies the server's leaf certificate matches the pinned
// SHA-256 fingerprint instead of relying on system CAs or hostname SANs.
func PinnedConfig(fingerprint string, nextProtos ...string) (*tls.Config, error) {
	want, err := ParseFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		// Verification is performed exclusively by leaf certificate SHA-256 pinning below.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         nextProtos,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("%w: no peer certificate", ErrPinMismatch)
			}
			got := sha256.Sum256(rawCerts[0])
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				return fmt.Errorf("%w: got %s, want %s", ErrPinMismatch, FormatFingerprint(rawCerts[0]), FormatFingerprintBytes(want))
			}
			return nil
		},
	}, nil
}

// FormatFingerprintBytes formats a 32-byte SHA-256 digest as "sha256:<64-lower-hex>".
func FormatFingerprintBytes(sum [32]byte) string {
	return "sha256:" + hex.EncodeToString(sum[:])
}
