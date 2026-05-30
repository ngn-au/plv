package main

import (
	"net"
	"testing"
)

func TestIPClassification(t *testing.T) {
	cases := []struct {
		ip   string
		priv bool
	}{
		{"192.168.8.5", true}, {"10.1.2.3", true}, {"172.16.0.1", true}, {"172.31.255.1", true},
		{"127.0.0.1", true}, {"169.254.1.1", true}, {"100.64.0.1", true}, {"::1", true}, {"fe80::1", true},
		{"198.51.100.86", false}, {"203.0.113.8", false}, {"192.0.2.7", false}, {"2001:db8::8888", false},
		{"", true}, {"not-an-ip", true}, // unknown/absent → treated internal
		{"172.32.0.1", false}, // just outside RFC1918 172.16/12
	}
	for _, c := range cases {
		if got := privateIP(c.ip); got != c.priv {
			t.Errorf("privateIP(%q) = %v, want %v", c.ip, got, c.priv)
		}
	}
}

func TestIPOf(t *testing.T) {
	cases := map[string]string{
		"mail.example.org[198.51.100.88]":           "198.51.100.88",
		"192.0.2.5[192.0.2.5]:25":                   "192.0.2.5",
		"mailbox.example.net[private/dovecot-lmtp]": "private/dovecot-lmtp", // non-IP bracket; privateIP() treats as internal
		"":      "",
		"local": "",
	}
	for in, want := range cases {
		if got := ipOf(in); got != want {
			t.Errorf("ipOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeliversLocal(t *testing.T) {
	yes := []Record{
		{Relay: "mailbox.example.net[private/dovecot-lmtp]"},
		{Relay: "local"},
		{Relay: "mail.example.com[127.0.0.1]:24", StatusDetail: "(250 2.0.0 <a@example.com> Saved)"},
		{Relay: "virtual"},
	}
	for _, r := range yes {
		if !deliversLocal(&r) {
			t.Errorf("deliversLocal(%+v) = false, want true", r)
		}
	}
	no := []Record{
		{Relay: "mx.example.com[198.51.100.20]:25"},
		{Relay: "127.0.0.1[127.0.0.1]:10024"}, // content-filter handoff is not local delivery
		{Relay: "192.168.8.5[192.168.8.5]:25"},
	}
	for _, r := range no {
		if deliversLocal(&r) {
			t.Errorf("deliversLocal(%+v) = true, want false", r)
		}
	}
}

// TestDirectionSingleLeg covers standalone setups (one record per message).
func TestDirectionSingleLeg(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		want string
	}{
		{
			name: "inbound MX: public client, unauth, local delivery",
			rec:  Record{Client: "mx.sender.example[198.51.100.86]", Relay: "local"},
			want: "inbound",
		},
		{
			name: "inbound via content filter: public client, relay to loopback",
			rec:  Record{Client: "mx.sender.example[198.51.100.86]", Relay: "127.0.0.1[127.0.0.1]:10024"},
			want: "inbound",
		},
		{
			name: "outbound: internal client relays to public MX",
			rec:  Record{Client: "unknown[192.168.8.25]", Relay: "mx.example.com[198.51.100.20]:25"},
			want: "outbound",
		},
		{
			name: "outbound: authenticated submission from a PUBLIC home IP → not inbound",
			rec:  Record{Client: "host.example.net[203.0.113.55]", Relay: "mx.example.com[203.0.113.1]:25", RawLines: []string{"postfix/submission/smtpd: sasl_username=user@example.com"}},
			want: "outbound",
		},
		{
			name: "internal: private client, local mailbox delivery",
			rec:  Record{Client: "unknown[192.168.8.25]", Relay: "mailbox[private/dovecot-lmtp]"},
			want: "internal",
		},
		{
			name: "internal: authenticated submission to a local mailbox",
			rec:  Record{Client: "host.example.net[203.0.113.55]", Relay: "local", RawLines: []string{"sasl_username=user@example.com"}},
			want: "internal",
		},
	}
	for _, c := range cases {
		if got := directionOf([]*Record{&c.rec}, nil); got != c.want {
			t.Errorf("%s: directionOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestDirectionRelayPair covers the gateway+mailbox correlation, where the inbound
// entry is only visible on the gateway leg and delivery on the other.
func TestDirectionRelayPair(t *testing.T) {
	// Inbound: gateway received from the internet (public, unauth) → loopback filter;
	// mailbox host received from gateway (private) → local delivery.
	gatewayIn := &Record{Client: "mx.sender.example[198.51.100.86]", Relay: "127.0.0.1[127.0.0.1]:10024"}
	mailbox := &Record{Client: "gateway[192.168.8.5]", Relay: "mailbox[private/dovecot-lmtp]"}
	if got := directionOf([]*Record{gatewayIn, mailbox}, nil); got != "inbound" {
		t.Errorf("relay-pair inbound: got %q, want inbound", got)
	}

	// Outbound: user submits to the mailbox host (auth, relays internally to the smart
	// host); the smart host relays out to a public MX.
	submit := &Record{Client: "unknown[192.168.8.25]", Relay: "gateway[192.168.8.25]:587", RawLines: []string{"sasl_username=user@example.com"}}
	smartHostOut := &Record{Client: "mailbox[192.168.8.25]", Relay: "mx.example.com[203.0.113.1]:25"}
	if got := directionOf([]*Record{submit, smartHostOut}, nil); got != "outbound" {
		t.Errorf("relay-pair outbound: got %q, want outbound", got)
	}

	// Internal: internal sender → internal mailbox, no public hop anywhere.
	intIn := &Record{Client: "unknown[192.168.8.25]", Relay: "gateway[192.168.8.5]:25"}
	intDeliver := &Record{Client: "mailbox[192.168.8.25]", Relay: "mailbox[private/dovecot-lmtp]"}
	if got := directionOf([]*Record{intIn, intDeliver}, nil); got != "internal" {
		t.Errorf("relay-pair internal: got %q, want internal", got)
	}
}

// TestDirectionRelayed: a true relay is ONE un-filtered queue id that received from a
// public client and sent straight back out to a public host. It must be distinguished
// from content-filter inbound to a public-IP backend (which only looks like a relay once
// the legs are merged), and from authenticated submission out.
func TestDirectionRelayed(t *testing.T) {
	// True relay (e.g. a PBX notification through a milter host out to a public MX): public
	// in, public out, one queue id, no local delivery, no content-filter merge.
	relay := &Record{Client: "pbx.example.com[203.0.113.9]", Relay: "mx.example.com[198.51.100.20]:25"}
	if got := directionOf([]*Record{relay}, nil); got != "relayed" {
		t.Errorf("true relay: got %q, want relayed", got)
	}

	// Content-filter inbound to a PUBLIC-IP backend: the accept/reinject merge sets
	// DeliveryQueueID, so it is the gateway carrying inbound mail onward — NOT a relay.
	cfMerged := &Record{Client: "sender.example.net[198.51.100.7]", Relay: "backend.example.com[203.0.113.50]:25", DeliveryQueueID: "ABC1230001"}
	if got := directionOf([]*Record{cfMerged}, nil); got != "inbound" {
		t.Errorf("content-filter inbound (public backend, merged): got %q, want inbound", got)
	}

	// Same, but detected via an orig_client= on the reinjected leg (no merge id on it).
	reinj := &Record{Client: "sender.example.net[198.51.100.8]", Relay: "backend.example.com[203.0.113.51]:25",
		RawLines: []string{"postfix/smtp[1]: X: orig_client=sender.example.net[198.51.100.8]"}}
	if got := directionOf([]*Record{reinj}, nil); got != "inbound" {
		t.Errorf("reinjected leg (orig_client): got %q, want inbound", got)
	}

	// Authenticated submission from a public IP relaying out is OUTBOUND, not relayed
	// (the public reception is an authenticated user sending, not internet transit).
	authOut := &Record{Client: "home.example.net[203.0.113.55]", Relay: "mx.example.com[203.0.113.1]:25",
		RawLines: []string{"postfix/submission/smtpd: sasl_username=user@example.com"}}
	if got := directionOf([]*Record{authOut}, nil); got != "outbound" {
		t.Errorf("authenticated submission: got %q, want outbound", got)
	}

	// A relay that ALSO delivers locally (forward) is inbound — it landed in a mailbox.
	relayAndLocal := []*Record{
		{Client: "pbx.example.com[203.0.113.9]", Relay: "mx.example.com[198.51.100.20]:25"},
		{Client: "pbx.example.com[203.0.113.9]", Relay: "local"},
	}
	if got := directionOf(relayAndLocal, nil); got != "inbound" {
		t.Errorf("relay+local delivery: got %q, want inbound", got)
	}
}

// TestDirectionWithPostfixConf: with mynetworks/local-domains, a content-filter gateway's
// own backend (a public IP in mynetworks) submitting mail that is relayed out is OUTBOUND
// — the mirror of the inbound-to-backend case — even though by IP alone it looks inbound.
func TestDirectionWithPostfixConf(t *testing.T) {
	_, backendNet, _ := net.ParseCIDR("203.0.113.0/24")
	pc := &PostfixConf{
		LocalDomains: map[string]bool{"mycorp.example": true},
		Networks:     []*net.IPNet{backendNet},
	}

	// Outbound: the backend (203.0.113.4 ∈ mynetworks) submitted it; it's content-filtered
	// (DeliveryQueueID set) and relayed out to a public MX.
	outFromBackend := []*Record{
		{Client: "remote.example-backend.net[203.0.113.4]", Relay: "127.0.0.1[127.0.0.1]:10023", DeliveryQueueID: "ABC1230001"},
		{Client: "localhost[127.0.0.1]", Relay: "mx.bigexternal.example[198.51.100.9]:25",
			RawLines: []string{"orig_client=remote.example-backend.net[203.0.113.4]"}},
	}
	if got := directionOf(outFromBackend, pc); got != "outbound" {
		t.Errorf("backend-submitted, relayed out: got %q, want outbound", got)
	}
	// Same shape but WITHOUT the conf → the old behaviour (mis-reads as inbound).
	if got := directionOf(outFromBackend, nil); got != "inbound" {
		t.Errorf("without conf the backend case still reads %q (expected the old inbound)", got)
	}

	// Inbound still inbound: an external sender (not in mynetworks) → content filter →
	// backend. Mirror of #72.
	inToBackend := []*Record{
		{Client: "mx.external-sender.example[198.51.100.20]", Relay: "127.0.0.1[127.0.0.1]:10024", DeliveryQueueID: "DEF4560001"},
		{Client: "localhost[127.0.0.1]", Relay: "remote.example-backend.net[203.0.113.4]:25",
			RawLines: []string{"orig_client=mx.external-sender.example[198.51.100.20]"}},
	}
	if got := directionOf(inToBackend, pc); got != "inbound" {
		t.Errorf("external → backend: got %q, want inbound", got)
	}

	// A trusted source sending from a local domain straight out (no content filter) is
	// outbound, NOT relayed (the trusted-PBX case).
	pbx := &Record{Client: "pbx.example.com[203.0.113.9]", Relay: "gmail.example[198.51.100.30]:25", From: "noreply@mycorp.example"}
	if got := directionOf([]*Record{pbx}, pc); got != "outbound" {
		t.Errorf("trusted PBX send: got %q, want outbound", got)
	}
}
