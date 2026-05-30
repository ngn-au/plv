package main

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// TestPKIEnrollRoundTrip exercises the full enrolment path with synthetic names:
// init CA -> server cert -> ticket -> node CSR -> receiver signs -> chain verifies.
func TestPKIEnrollRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if err := pkiInit(dir); err != nil {
		t.Fatalf("pkiInit: %v", err)
	}
	if err := pkiInit(dir); err == nil {
		t.Error("pkiInit should refuse to overwrite an existing CA")
	}
	if err := pkiServer(dir, []string{"mon.example.com", "192.0.2.10"}); err != nil {
		t.Fatalf("pkiServer: %v", err)
	}

	// Ticket binds to a single CN.
	salt, err := loadTicketSalt(dir)
	if err != nil {
		t.Fatalf("loadTicketSalt: %v", err)
	}
	tk := ticketFor(salt, "node-a")
	if !validTicket(salt, "node-a", tk) {
		t.Fatal("validTicket rejected a good ticket")
	}
	if validTicket(salt, "node-b", tk) {
		t.Fatal("a ticket for node-a must not validate for node-b")
	}

	// Node generates its own key+CSR; receiver signs it.
	csrPEM, keyPEM, err := generateCSR("node-a")
	if err != nil {
		t.Fatalf("generateCSR: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("generateCSR returned an empty private key")
	}
	certPEM, err := signCSR(dir, csrPEM, "node-a")
	if err != nil {
		t.Fatalf("signCSR: %v", err)
	}
	if _, err := signCSR(dir, csrPEM, "node-b"); err == nil {
		t.Error("signCSR must reject a CSR whose CN differs from the ticketed name")
	}

	// Signed cert: CN=node-a, chains to the CA, usable for client auth.
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("signed cert is not valid PEM")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse signed cert: %v", err)
	}
	if leaf.Subject.CommonName != "node-a" {
		t.Errorf("signed cert CN = %q, want node-a", leaf.Subject.CommonName)
	}
	caCert, _, err := loadCA(dir)
	if err != nil {
		t.Fatalf("loadCA: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("signed client cert does not verify against the CA: %v", err)
	}

	if !pkiReady(dir) {
		t.Error("pkiReady should be true once CA + server cert/key exist")
	}
}
