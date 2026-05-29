package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadPostfixConf builds a synthetic /etc/postfix tree (generated, never ./logs) and
// checks the derivation: inline + file-backed domains and networks, $var expansion, IPv6
// bracket CIDRs, and that database-backed tables (mysql/proxy) are skipped.
func TestLoadPostfixConf(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.cf", `# synthetic
mydomain = corp.example
myhostname = mx1.corp.example
mydestination = localhost, $myhostname, mail.corp.example
relay_domains = hash:/etc/postfix/relay_domains
virtual_mailbox_domains = proxy:mysql:/etc/postfix/vdomains.cf
mynetworks = 127.0.0.0/8 168.0.0.0/8 [::1]/128
    10.20.0.0/24 cidr:/etc/postfix/trusted_nets
`)
	// referenced by absolute path in main.cf; resolved by basename inside dir
	write("relay_domains", "# managed by tooling\nclienta.example     OK\nclientb.example     OK\n")
	write("trusted_nets", "203.0.113.0/24   permit\n198.51.100.7/32  permit\n")

	pc, err := loadPostfixConf(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"corp.example", "mx1.corp.example", "mail.corp.example", "clienta.example", "clientb.example"} {
		if !pc.LocalDomains[d] {
			t.Errorf("missing local domain %q (have %v)", d, pc.LocalDomains)
		}
	}
	if pc.LocalDomains["localhost"] {
		t.Error("localhost must not be treated as a local domain")
	}
	if len(pc.LocalDomains) != 5 {
		t.Errorf("local domains = %v, want exactly the 5 real ones (mysql table skipped)", pc.LocalDomains)
	}

	// isLocalDomain: exact match, subdomain match, and a stranger.
	if !pc.isLocalDomain("user@clienta.example") {
		t.Error("clienta.example should be local")
	}
	if !pc.isLocalDomain("x@deep.sub.corp.example") {
		t.Error("a subdomain of corp.example should be local")
	}
	if pc.isLocalDomain("user@stranger.example") {
		t.Error("stranger.example must not be local")
	}

	// mynetworks: specific ranges (/24, /32) and the IPv6 /128 are kept.
	for _, ip := range []string{"::1", "10.20.0.5", "203.0.113.9", "198.51.100.7"} {
		if !pc.inMyNetworks(ip) {
			t.Errorf("%s should be inside mynetworks", ip)
		}
	}
	// Over-broad IPv4 /8 entries are dropped (the safety cap): a stray public 168.0.0.0/8
	// must not trust external senders, and loopback 127.0.0.0/8 is handled elsewhere.
	for _, ip := range []string{"168.0.0.1", "127.0.0.1", "198.51.100.99", "198.51.100.8", "198.51.100.250", "10.20.1.1"} {
		if pc.inMyNetworks(ip) {
			t.Errorf("%s should NOT be inside mynetworks (over-broad /8 dropped, or simply outside)", ip)
		}
	}
}

// TestPostfixConfNil: a nil PostfixConf is inert (the heuristic-only path).
func TestPostfixConfNil(t *testing.T) {
	var pc *PostfixConf
	if pc.inMyNetworks("203.0.113.9") || pc.isLocalDomain("a@corp.example") {
		t.Error("nil PostfixConf must report false for everything")
	}
}
