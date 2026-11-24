package listen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// TLS file names inside the TLS directory.
const (
	CAFile      = "ca.pem"
	CAKeyFile   = "ca-key.pem"
	LeafFile    = "cert.pem"
	LeafKeyFile = "key.pem"
)

// RenewBefore is how close to expiry a leaf gets regenerated.
const RenewBefore = 30 * 24 * time.Hour

// TLSOptions controls certificate loading/generation.
type TLSOptions struct {
	Dir         string   // e.g. ~/.config/htadb/tls
	CertFile    string   // explicit --tls-cert (takes precedence)
	KeyFile     string   // explicit --tls-key
	ExtraSANs   []string // --tls-san values (DNS names or IPs)
	ListenSpecs []Spec   // https listeners, for their IPs
	Force       bool     // regenerate the leaf unconditionally
	Now         func() time.Time
	Logger      *slog.Logger
}

func (o *TLSOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// DefaultTLSDir is <UserConfigDir>/htadb/tls, or <configDir>/tls when set.
func DefaultTLSDir(configDir string) (string, error) {
	if configDir != "" {
		return filepath.Join(configDir, "tls"), nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "htadb", "tls"), nil
}

// TLSConfig loads explicit certificates or generates/renews the local CA and
// leaf, then returns a server TLS config.
func TLSConfig(o TLSOptions) (*tls.Config, error) {
	if o.CertFile != "" || o.KeyFile != "" {
		if o.CertFile == "" || o.KeyFile == "" {
			return nil, errors.New("tls: --tls-cert and --tls-key must be given together")
		}
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
	}
	if _, err := EnsureLeaf(o); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(o.Dir, LeafFile), filepath.Join(o.Dir, LeafKeyFile))
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// WantedSANs returns the sorted SAN list for the leaf.
func WantedSANs(o TLSOptions) []string {
	set := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if h, err := os.Hostname(); err == nil && h != "" {
		set[strings.ToLower(h)] = true
	}
	for _, s := range o.ListenSpecs {
		if s.Scheme != "https" {
			continue
		}
		if ip := s.IP(); ip != nil {
			set[ip.String()] = true
		} else if s.IsUnspecified() {
			for _, ip := range localIPs() {
				set[ip.String()] = true
			}
		} else if host, _, _ := net.SplitHostPort(s.Addr); host != "" {
			set[strings.ToLower(host)] = true
		}
	}
	for _, s := range o.ExtraSANs {
		if s = strings.TrimSpace(s); s != "" {
			if ip := net.ParseIP(s); ip != nil {
				s = ip.String()
			} else {
				s = strings.ToLower(s)
			}
			set[s] = true
		}
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func localIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP)
		}
	}
	return out
}

func certSANs(c *x509.Certificate) []string {
	var out []string
	for _, d := range c.DNSNames {
		out = append(out, strings.ToLower(d))
	}
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out
}

// EnsureCA loads the CA from dir or creates it once.
func EnsureCA(dir string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, bool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, false, err
	}
	os.Chmod(dir, 0o700)
	cp, kp := filepath.Join(dir, CAFile), filepath.Join(dir, CAKeyFile)
	if cert, key, err := loadPair(cp, kp); err == nil {
		return cert, key, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false, fmt.Errorf("tls: loading CA: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, false, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: "htadb local CA (" + host + ")", Organization: []string{"htadb"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, false, err
	}
	if err := writePair(cp, kp, der, key); err != nil {
		return nil, nil, false, err
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key, true, nil
}

// EnsureLeaf makes sure a valid leaf for the wanted SANs exists; it returns
// true when a new leaf was written.
func EnsureLeaf(o TLSOptions) (bool, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	now := o.now()
	ca, caKey, created, err := EnsureCA(o.Dir, now)
	if err != nil {
		return false, err
	}
	if created {
		log.Info("created local CA; trust it to avoid certificate warnings (see `htadb tls print-ca`)", "path", filepath.Join(o.Dir, CAFile))
	}
	want := WantedSANs(o)
	lp, lkp := filepath.Join(o.Dir, LeafFile), filepath.Join(o.Dir, LeafKeyFile)
	if !o.Force {
		if leaf, _, err := loadPair(lp, lkp); err == nil {
			reason := ""
			switch {
			case !slices.Equal(certSANs(leaf), want):
				reason = "SANs changed"
			case now.Add(RenewBefore).After(leaf.NotAfter):
				reason = "expires soon"
			case leaf.CheckSignatureFrom(ca) != nil:
				reason = "not signed by current CA"
			}
			if reason == "" {
				return false, nil
			}
			log.Info("regenerating TLS leaf certificate", "reason", reason)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(),
		Subject:      pkix.Name{CommonName: "htadb", Organization: []string{"htadb"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, 365),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, s := range want {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return false, err
	}
	// Leaf file carries the chain so clients that trust the CA verify it.
	if err := writePair(lp, lkp, der, key, ca.Raw); err != nil {
		return false, err
	}
	return true, nil
}

// CAInfo returns the CA path and its SHA-256 fingerprint.
func CAInfo(dir string) (string, string, error) {
	p := filepath.Join(dir, CAFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return p, "", err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return p, "", errors.New("tls: bad CA PEM")
	}
	sum := sha256.Sum256(blk.Bytes)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return p, strings.Join(parts, ":"), nil
}

func randSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func loadPair(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cblk, _ := pem.Decode(cb)
	kblk, _ := pem.Decode(kb)
	if cblk == nil || kblk == nil {
		return nil, nil, errors.New("bad PEM")
	}
	cert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kblk.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func writePair(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey, chain ...[]byte) error {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	var cp []byte
	cp = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for _, c := range chain {
		cp = append(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c})...)
	}
	if err := writeFile0600(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})); err != nil {
		return err
	}
	return writeFile0600(certPath, cp)
}

func writeFile0600(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	os.Chmod(tmp, 0o600)
	return os.Rename(tmp, p)
}
