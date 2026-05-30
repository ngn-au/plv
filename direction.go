package main

// Mail direction classification — INBOUND / OUTBOUND / INTERNAL — derived purely from
// Postfix log facts (public/private client & relay IPs, SASL authentication, and
// local-delivery markers). It is general to any setup (standalone, milter, content
// filter, relay pair); it never assumes specific server roles. A message's direction
// is computed across ALL its legs (records sharing a Message-ID), since the inbound
// entry can only be seen on the gateway leg while delivery happens on another.

import (
	"net"
	"regexp"
	"strings"
)

var reBracketIP = regexp.MustCompile(`\[([^\]]+)\]`)

// ipOf extracts the bracketed IP from a "host[ip]" or "host[ip]:port" token.
func ipOf(hostBracket string) string {
	m := reBracketIP.FindStringSubmatch(hostBracket)
	if m == nil {
		return ""
	}
	return m[1]
}

// privateIP reports whether ip is private/loopback/link-local/CGNAT — or unparseable
// (an absent or non-IP client/relay, e.g. a local pipe, is treated as internal).
func privateIP(ip string) bool {
	a := net.ParseIP(ip)
	if a == nil {
		return true
	}
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsPrivate() {
		return true
	}
	if v4 := a.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true // CGNAT 100.64.0.0/10
	}
	return false
}

func publicIP(ip string) bool { return ip != "" && !privateIP(ip) }

// deliversLocal reports a terminal local-mailbox delivery rather than an onward or
// external relay. Covers the common Postfix/Dovecot/Cyrus forms: lmtp/dovecot/virtual/
// local transports, and the status-detail phrasings ("… Saved", "delivered to
// mailbox/maildir/command/file").
func deliversLocal(r *Record) bool {
	relay := strings.ToLower(r.Relay)
	if strings.Contains(relay, "dovecot") || strings.Contains(relay, "lmtp") ||
		strings.Contains(relay, "virtual") || strings.Contains(relay, "cyrus") ||
		strings.Contains(relay, "maildrop") || relay == "local" || strings.HasPrefix(relay, "local[") {
		return true
	}
	sd := strings.ToLower(r.StatusDetail)
	return strings.Contains(sd, " saved") ||
		strings.Contains(sd, "delivered to mailbox") || strings.Contains(sd, "delivered to maildir") ||
		strings.Contains(sd, "delivered to command") || strings.Contains(sd, "delivered to file")
}

// legReceivedPublicUnauth: this leg accepted the message from a PUBLIC client over the
// MX path with no SASL auth — the signature of inbound mail from the internet. An
// authenticated submission from a public IP is a user sending, NOT inbound; likewise a
// client inside the Postfix mynetworks (when a PostfixConf is supplied) is a TRUSTED
// source — e.g. a content-filter gateway's own backend on a public IP submitting
// OUTBOUND mail — so it is not inbound reception either.
func legReceivedPublicUnauth(r *Record, pc *PostfixConf) bool {
	ip := ipOf(r.Client)
	if !publicIP(ip) {
		return false
	}
	if pc.inMyNetworks(ip) {
		return false
	}
	for _, l := range r.RawLines {
		if strings.Contains(l, "sasl_username=") {
			return false
		}
	}
	return true
}

// legSendsPublic: this leg relayed the message out to a PUBLIC host that is NOT one of
// our own (an external MX), i.e. it left our control. Loopback content-filter handoffs
// and local delivery don't count (loopback is private; local delivery is terminal). A
// relay to a host inside mynetworks is delivery to our OWN backend — internal, not an
// external send — so it doesn't count either.
func legSendsPublic(r *Record, pc *PostfixConf) bool {
	ip := ipOf(r.Relay)
	return publicIP(ip) && !deliversLocal(r) && !pc.inMyNetworks(ip)
}

// isRelayLeg reports a pure pass-through hop: a SINGLE leg that both received the message
// from a public, unauthenticated client AND relayed it straight back out to a public host.
// This is the reliable signal for "relayed" (external→external transit). It must be
// per-leg, NOT aggregated: a content-filter gateway (PMG) receives inbound mail from the
// internet and re-injects it to a customer mailserver that itself has a PUBLIC IP — so
// the public reception and the public send land on SEPARATE legs (split by the loopback
// filter). Aggregating would wrongly flag all such inbound mail as relayed. A genuine
// relay does both in one queue id.
func isRelayLeg(r *Record, pc *PostfixConf) bool {
	// A content-filter accept/reinject merge (DeliveryQueueID set, or an orig_client= on
	// the leg) means this is the gateway carrying INBOUND mail onward to its backend
	// mailserver — even when that backend has a public IP — NOT transit traffic. Only a
	// single un-filtered queue id that both received from the internet and sent straight
	// back out is a true relay.
	return legReceivedPublicUnauth(r, pc) && legSendsPublic(r, pc) && r.DeliveryQueueID == "" && !reinjected(r)
}

// reinjected reports that this leg is the output of a local content-filter re-injection
// (it carries an orig_client=). On such a leg Postfix logs client=localhost and the real
// internet sender as orig_client=, which PLV surfaces as Client — so the leg looks like a
// public→public hop even though it is the gateway delivering inbound mail onward.
func reinjected(r *Record) bool {
	for _, l := range r.RawLines {
		if strings.Contains(l, "orig_client=") {
			return true
		}
	}
	return false
}

// classifyDirection maps the per-message signals onto a direction. A message with a relay
// leg and no local delivery merely PASSED THROUGH us — that's "relayed" (external→
// external). Otherwise: inbound if received from the internet, outbound if sent to it.
// When neither side is decisive by IP, the configured local/hosted domains break the tie
// (to a local domain → inbound; from a local domain → outbound). Else internal. Derived
// purely from log facts + the Postfix config, so it generalises across standalone /
// milter / content-filter / relay-pair setups.
func classifyDirection(in, out, local, relay, fromLocal, toLocal bool) string {
	switch {
	case relay && !local:
		return "relayed"
	case in:
		return "inbound"
	case out:
		return "outbound"
	case toLocal && !fromLocal:
		return "inbound"
	case fromLocal && !toLocal:
		return "outbound"
	default:
		return "internal"
	}
}

// directionResolver returns a closure giving any record's direction, with the signals
// aggregated per Message-ID across all legs (so the gateway leg's public reception flows
// to every leg of the message). Caller holds the read lock; the maps are built once over
// the whole record set. Used by both Search and Stats.
func (s *Store) directionResolver() func(*Record) string {
	midIn := make(map[string]bool)
	midOut := make(map[string]bool)
	midLocal := make(map[string]bool)
	midRelay := make(map[string]bool)
	midFromLocal := make(map[string]bool)
	midToLocal := make(map[string]bool)
	for i := range s.records {
		rr := &s.records[i]
		if rr.Subsumed || rr.MessageID == "" {
			continue
		}
		pc := s.confFor(rr.Origin) // each leg uses its own server's config (distributed)
		if legReceivedPublicUnauth(rr, pc) {
			midIn[rr.MessageID] = true
		}
		if legSendsPublic(rr, pc) {
			midOut[rr.MessageID] = true
		}
		if deliversLocal(rr) {
			midLocal[rr.MessageID] = true
		}
		if isRelayLeg(rr, pc) {
			midRelay[rr.MessageID] = true
		}
		if pc.isLocalDomain(rr.From) {
			midFromLocal[rr.MessageID] = true
		}
		if pc.isLocalDomain(rr.To) {
			midToLocal[rr.MessageID] = true
		}
	}
	return func(r *Record) string {
		pc := s.confFor(r.Origin)
		in, out, local, relay := legReceivedPublicUnauth(r, pc), legSendsPublic(r, pc), deliversLocal(r), isRelayLeg(r, pc)
		fromLocal, toLocal := pc.isLocalDomain(r.From), pc.isLocalDomain(r.To)
		if r.MessageID != "" {
			in, out, local, relay = midIn[r.MessageID], midOut[r.MessageID], midLocal[r.MessageID], midRelay[r.MessageID]
			fromLocal, toLocal = midFromLocal[r.MessageID], midToLocal[r.MessageID]
		}
		return classifyDirection(in, out, local, relay, fromLocal, toLocal)
	}
}

// directionOf classifies a message from its legs (see classifyDirection). pc may be nil.
func directionOf(legs []*Record, pc *PostfixConf) string {
	in, out, local, relay, fromLocal, toLocal := false, false, false, false, false, false
	for _, r := range legs {
		if legReceivedPublicUnauth(r, pc) {
			in = true
		}
		if legSendsPublic(r, pc) {
			out = true
		}
		if deliversLocal(r) {
			local = true
		}
		if isRelayLeg(r, pc) {
			relay = true
		}
		if pc.isLocalDomain(r.From) {
			fromLocal = true
		}
		if pc.isLocalDomain(r.To) {
			toLocal = true
		}
	}
	return classifyDirection(in, out, local, relay, fromLocal, toLocal)
}
