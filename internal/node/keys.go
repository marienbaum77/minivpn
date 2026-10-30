// Package node renders everything a fresh server needs to become a node:
// the sing-box config and the setup script that installs sing-box and the
// watchdog that deletes the server when the session is over.
package node

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
)

// HY2ServerName is the name in the node's self-signed Hysteria2 certificate.
// The client pins the certificate itself, and salamander hides the TLS
// handshake, so the name is never seen on the wire.
const HY2ServerName = "node.minivpn"

// Keys identify the user and the node to each other. They are generated once
// and reused by every node, so only the address changes between sessions.
type Keys struct {
	UUID       string `json:"uuid"`
	PrivateKey string `json:"private_key"` // Reality x25519, base64url
	PublicKey  string `json:"public_key"`
	ShortID    string `json:"short_id"`

	// Hysteria2: the user's password, the salamander obfuscation password
	// and the node's TLS certificate, which the client pins.
	HY2Password string `json:"hy2_password"`
	HY2Obfs     string `json:"hy2_obfs"`
	CertPEM     string `json:"cert_pem"`
	CertKeyPEM  string `json:"cert_key_pem"`
}

func NewKeys() (Keys, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Keys{}, err
	}
	sid := make([]byte, 8)
	if _, err := rand.Read(sid); err != nil {
		return Keys{}, err
	}
	k := Keys{
		UUID:       uuid.NewString(),
		PrivateKey: base64.RawURLEncoding.EncodeToString(priv.Bytes()),
		PublicKey:  base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		ShortID:    hex.EncodeToString(sid),
	}
	return k, k.AddHysteria()
}

// AddHysteria fills the Hysteria2 keys; settings saved before Hysteria2 have
// only the Reality ones.
func (k *Keys) AddHysteria() error {
	var err error
	if k.HY2Password, err = randomHex(16); err != nil {
		return err
	}
	if k.HY2Obfs, err = randomHex(16); err != nil {
		return err
	}
	k.CertPEM, k.CertKeyPEM, err = selfSigned()
	return err
}

// PhoneUUID returns a deterministic UUID for phone apps (Happ, v2rayNG),
// giving phones their own user on the node.
func (k Keys) PhoneUUID() string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(k.UUID+"-phone")).String()
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func selfSigned() (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: HY2ServerName},
		DNSNames:              []string{HY2ServerName},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // its own root: the client trusts exactly this one
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), nil
}

// Validate catches keys edited by hand: a wrong public key makes every
// handshake fail with nothing useful in the logs.
func (k Keys) Validate() error {
	if _, err := uuid.Parse(k.UUID); err != nil {
		return fmt.Errorf("uuid: %w", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(k.PrivateKey)
	if err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	if base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()) != k.PublicKey {
		return fmt.Errorf("public key does not match the private key")
	}
	if _, err := hex.DecodeString(k.ShortID); err != nil || len(k.ShortID) > 16 {
		return fmt.Errorf("short id must be up to 16 hex chars")
	}
	if k.HY2Password == "" || k.HY2Obfs == "" {
		return fmt.Errorf("hysteria2 passwords are missing")
	}
	if _, err := tls.X509KeyPair([]byte(k.CertPEM), []byte(k.CertKeyPEM)); err != nil {
		return fmt.Errorf("hysteria2 certificate: %w", err)
	}
	return nil
}
