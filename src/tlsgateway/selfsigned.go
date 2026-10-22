package tlsgateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"sync"
	"time"
)

// selfSignedValidity is how long a generated certificate is valid.
const selfSignedValidity = 30 * 24 * time.Hour

// SelfSigned makes a certificate per hostname on first use and keeps it in
// memory. Browsers warn about it; it exists for development and tests only
// (MO_TUNNEL_TLS_SELF_SIGNED).
type SelfSigned struct {
	mu    sync.Mutex
	certs map[string]*tls.Certificate
}

func NewSelfSigned() *SelfSigned {
	return &SelfSigned{certs: map[string]*tls.Certificate{}}
}

func (s *SelfSigned) Certificate(hostname string) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cert, ok := s.certs[hostname]; ok && time.Now().Before(cert.Leaf.NotAfter) {
		return cert, nil
	}
	cert, err := selfSignedCertificate(hostname, time.Now())
	if err != nil {
		return nil, err
	}
	s.certs[hostname] = cert
	return cert, nil
}

// Challenge: a self-signed setup never validates anything.
func (s *SelfSigned) Challenge(string) (*tls.Certificate, bool) { return nil, false }

// Status: a self-signed certificate is made on the spot.
func (s *SelfSigned) Status(string) CertificateStatus {
	return CertificateStatus{State: CertificateReady, Message: "self-signed certificate (development only)"}
}

func (s *SelfSigned) Forget(_ context.Context, hostname string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.certs, hostname)
	return nil
}

func selfSignedCertificate(hostname string, now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hostname},
		DNSNames:              []string{hostname},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
