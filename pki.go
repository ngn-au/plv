package main

// Minimal in-house PKI for the distributed mode, stdlib only. The receiver holds
// a CA (ca.pem/ca-key.pem) and a server cert (cert.pem/key.pem); each forwarder
// enrols to get its own client cert (cert.pem/key.pem, CN = its server name).
// Enrolment is authorised by an HMAC ticket (Icinga-style), so a forwarder's
// private key never leaves the node. All files live flat in PLV_DATA_DIR.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"
	certFile   = "cert.pem"
	keyFile    = "key.pem"
	saltFile   = "ticket-salt"

	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 5 * 365 * 24 * time.Hour
)

func dataPath(dataDir, name string) string { return filepath.Join(dataDir, name) }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pkiReady reports whether the data dir holds the cert material both roles need
// (CA + this node's cert/key). Used to gracefully fall back to standalone.
func pkiReady(dataDir string) bool {
	return fileExists(dataPath(dataDir, caCertFile)) &&
		fileExists(dataPath(dataDir, certFile)) &&
		fileExists(dataPath(dataDir, keyFile))
}

func genKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func writeCertPEM(path string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey) error {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b}), 0o600)
}

func loadCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM certificate", path)
	}
	return x509.ParseCertificate(blk.Bytes)
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM key", path)
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

func loadCA(dataDir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := loadCert(dataPath(dataDir, caCertFile))
	if err != nil {
		return nil, nil, err
	}
	key, err := loadKey(dataPath(dataDir, caKeyFile))
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// pkiInit creates the CA and a random ticket salt. Refuses to clobber an existing CA.
func pkiInit(dataDir string) error {
	if fileExists(dataPath(dataDir, caCertFile)) {
		return fmt.Errorf("CA already exists at %s (refusing to overwrite)", dataPath(dataDir, caCertFile))
	}
	// 0o700: this dir holds the CA private key and the ticket salt — operator-only.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	key, err := genKey()
	if err != nil {
		return err
	}
	serial, err := randSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "PLV CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := writeCertPEM(dataPath(dataDir, caCertFile), der); err != nil {
		return err
	}
	if err := writeKeyPEM(dataPath(dataDir, caKeyFile), key); err != nil {
		return err
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	return os.WriteFile(dataPath(dataDir, saltFile), []byte(hex.EncodeToString(salt)), 0o600)
}

// pkiServer issues the receiver's server cert (cert.pem/key.pem) for the given
// hostnames/IPs (SANs), signed by the CA.
func pkiServer(dataDir string, hosts []string) error {
	caCert, caKey, err := loadCA(dataDir)
	if err != nil {
		return fmt.Errorf("load CA (run 'plv pki init' first): %w", err)
	}
	key, err := genKey()
	if err != nil {
		return err
	}
	serial, err := randSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writeCertPEM(dataPath(dataDir, certFile), der); err != nil {
		return err
	}
	return writeKeyPEM(dataPath(dataDir, keyFile), key)
}

// generateCSR makes a fresh key + CSR for a forwarder (CN = its server name). The
// key stays on the node; only the CSR is sent to the receiver's /enroll endpoint.
func generateCSR(cn string) (csrPEM, keyPEM []byte, err error) {
	key, err := genKey()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, err
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return csrPEM, keyPEM, nil
}

// signCSR validates a CSR and signs it as a client cert, forcing CN == expectCN
// (the CN the ticket authorised). Returns the signed cert PEM.
func signCSR(dataDir string, csrPEM []byte, expectCN string) ([]byte, error) {
	caCert, caKey, err := loadCA(dataDir)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(csrPEM)
	if blk == nil {
		return nil, fmt.Errorf("no PEM block in CSR")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	if csr.Subject.CommonName != expectCN {
		return nil, fmt.Errorf("CSR CN %q does not match the ticketed name %q", csr.Subject.CommonName, expectCN)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: expectCN},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func loadTicketSalt(dataDir string) ([]byte, error) {
	b, err := os.ReadFile(dataPath(dataDir, saltFile))
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimSpace(string(b)))
}

// ticketFor is the enrolment token for a CN: HMAC-SHA256(salt, CN). Stateless, so
// a ticket stays valid until the salt is rotated.
func ticketFor(salt []byte, cn string) string {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(cn))
	return hex.EncodeToString(mac.Sum(nil))
}

func validTicket(salt []byte, cn, ticket string) bool {
	return hmac.Equal([]byte(ticketFor(salt, cn)), []byte(ticket))
}

// certFingerprint is the SHA-256 of the cert DER, colon-separated hex — shown for
// out-of-band TOFU verification during enrolment.
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// serverTLSConfig is the receiver ingest listener's TLS: present the server cert,
// verify client certs against the CA when offered. The /enrol path carries no
// client cert (VerifyClientCertIfGiven allows that); /ingest requires one.
func serverTLSConfig(dataDir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(dataPath(dataDir, certFile), dataPath(dataDir, keyFile))
	if err != nil {
		return nil, err
	}
	pool, err := caPool(dataDir)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// clientTLSConfig is the forwarder's TLS: present its client cert, trust the CA.
func clientTLSConfig(dataDir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(dataPath(dataDir, certFile), dataPath(dataDir, keyFile))
	if err != nil {
		return nil, err
	}
	pool, err := caPool(dataDir)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func caPool(dataDir string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(dataPath(dataDir, caCertFile))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s: not a valid CA certificate", dataPath(dataDir, caCertFile))
	}
	return pool, nil
}

// runPKI handles the `plv pki <init|server|ticket>` subcommands.
func runPKI(dataDir string, args []string) {
	if len(args) == 0 {
		pkiUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "init":
		if err := pkiInit(dataDir); err != nil {
			fatalErr(err)
		}
		fmt.Printf("CA initialised in %s (ca.pem, ca-key.pem, ticket-salt)\n", dataDir)
		fmt.Println("Next: plv pki server <hostname-or-ip>...   then enrol nodes with: plv pki ticket --cn <name>")
	case "server":
		hosts := args[1:]
		if len(hosts) == 0 {
			fmt.Fprintln(os.Stderr, "usage: plv pki server <hostname-or-ip>...")
			os.Exit(1)
		}
		if err := pkiServer(dataDir, hosts); err != nil {
			fatalErr(err)
		}
		fmt.Printf("server cert written (cert.pem, key.pem) for %s\n", strings.Join(hosts, ", "))
		if cert, err := loadCert(dataPath(dataDir, certFile)); err == nil {
			fmt.Printf("server cert fingerprint: SHA256:%s\n", certFingerprint(cert.Raw))
		}
	case "ticket":
		cn := flagValue(args[1:], "--cn")
		if cn == "" {
			fmt.Fprintln(os.Stderr, "usage: plv pki ticket --cn <name>")
			os.Exit(1)
		}
		salt, err := loadTicketSalt(dataDir)
		if err != nil {
			fatalErr(fmt.Errorf("load ticket salt (run 'plv pki init' first): %w", err))
		}
		fmt.Printf("CN:     %s\n", cn)
		fmt.Printf("ticket: %s\n", ticketFor(salt, cn))
		fmt.Printf("\nGive the node operator this ticket plus a copy of the CA cert:\n  %s\n", dataPath(dataDir, caCertFile))
		fmt.Printf("They enrol with:\n  plv enroll --to https://<this-receiver>:<port> --cn %s --ticket <ticket> --ca ca.pem\n", cn)
	default:
		pkiUsage()
		os.Exit(1)
	}
}

func pkiUsage() {
	fmt.Fprintln(os.Stderr, "usage: plv pki <init | server <host>... | ticket --cn <name>>")
}

// flagValue pulls "--name value" or "--name=value" out of a small arg slice.
func flagValue(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], name+"=") {
			return strings.TrimPrefix(args[i], name+"=")
		}
	}
	return ""
}

func fatalErr(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
