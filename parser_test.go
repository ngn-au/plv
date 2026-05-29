package main

import (
	"strings"
	"sync"
	"testing"
)

// captureSink records everything a forwarder would ship.
type captureSink struct {
	mu   sync.Mutex
	recs []Record
}

func (c *captureSink) Enqueue(r []Record) {
	c.mu.Lock()
	c.recs = append(c.recs, r...)
	c.mu.Unlock()
}

func (c *captureSink) has(qid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.recs {
		if r.QueueID == qid {
			return true
		}
	}
	return false
}

// TestForwarderSinkReceivesNewRecords guards the forwarder path: a brand-new
// message (the common case — first time a queue id is seen) must be handed to the
// sink. Regression test for the new-record branch only enqueuing when a DB was set.
func TestForwarderSinkReceivesNewRecords(t *testing.T) {
	store := NewStore(nil) // no DB, like a forwarder
	sink := &captureSink{}
	store.SetSink(sink)
	store.AddRecords(parseLines([]string{
		`2026-01-02T01:51:46.000000+00:00 mailhost postfix/smtp[1]: F1F1F1F101: to=<r@example.net>, relay=mx.example.net[203.0.113.40]:25, dsn=2.0.0, status=sent (250 ok)`,
		`2026-01-02T01:51:46.400000+00:00 mailhost postfix/qmgr[1]: F1F1F1F101: removed`,
	}))
	if !sink.has("F1F1F1F101") {
		t.Fatal("forwarder sink never received a brand-new record (new-record branch must enqueue when a sink is set)")
	}
}

// All sample log lines below use synthetic data only: example.com/.net/.org
// addresses, RFC 5737 documentation IPs (192.0.2.0/24, 198.51.100.0/24,
// 203.0.113.0/24) and made-up queue ids.

// recordByQID is a small test helper.
func recordByQID(records []Record, qid string) *Record {
	for i := range records {
		if records[i].QueueID == qid {
			return &records[i]
		}
	}
	return nil
}

// TestPMGSpamQuarantine covers the headline case: Postfix logs status=sent to the
// local content filter, but the filter quarantined the message as spam. The record
// must end up as "spam", not the misleading green "sent".
func TestPMGSpamQuarantine(t *testing.T) {
	lines := []string{
		`2026-01-02T00:13:25.514530+00:00 mailhost pmg-smtp-filter[1001]: C0FFEE0000000001: new mail message-id=<spam001@example.org>`,
		`2026-01-02T00:13:26.177019+00:00 mailhost pmg-smtp-filter[1001]: C0FFEE0000000001: SA score=8/5 time=0.5 bayes=0.00 hits=FROM_SUSPICIOUS_NTLD(0.5)`,
		`2026-01-02T00:13:26.191069+00:00 mailhost pmg-smtp-filter[1001]: C0FFEE0000000001: moved mail for <recipient@example.net> to spam quarantine - B2B2B2B202 (rule: Quarantine/Mark Spam (Level 3))`,
		`2026-01-02T00:13:26.195961+00:00 mailhost postfix/lmtp[1002]: A1A1A1A101: to=<recipient@example.net>, relay=127.0.0.1[127.0.0.1]:10024, delay=7.8, dsn=2.5.0, status=sent (250 2.5.0 OK (C0FFEE0000000001))`,
		`2026-01-02T00:13:26.196000+00:00 mailhost postfix/qmgr[1003]: A1A1A1A101: removed`,
	}
	recs := parseLines(lines)
	r := recordByQID(recs, "A1A1A1A101")
	if r == nil {
		t.Fatal("no record for queue id A1A1A1A101")
	}
	if r.Status != "sent" {
		t.Errorf("raw Status = %q, want %q", r.Status, "sent")
	}
	if r.Disposition != "spam" {
		t.Errorf("Disposition = %q, want %q", r.Disposition, "spam")
	}
	if r.SpamScore != "8/5" {
		t.Errorf("SpamScore = %q, want %q", r.SpamScore, "8/5")
	}
	if r.FilterRule != "Quarantine/Mark Spam (Level 3)" {
		t.Errorf("FilterRule = %q, want %q", r.FilterRule, "Quarantine/Mark Spam (Level 3)")
	}
	if r.FilterAction != "spam quarantine" {
		t.Errorf("FilterAction = %q, want %q", r.FilterAction, "spam quarantine")
	}
	if r.FilterID != "C0FFEE0000000001" {
		t.Errorf("FilterID = %q, want %q", r.FilterID, "C0FFEE0000000001")
	}
}

// TestPMGAccept: filter accepted the message; it stays delivered/green ("sent"),
// but we still record the scanner action and score for the detail view.
func TestPMGAccept(t *testing.T) {
	lines := []string{
		`2026-01-02T00:06:47.354481+00:00 mailhost pmg-smtp-filter[1004]: C0FFEE0000000002: SA score=0/5 time=0.5 hits=BAYES_00(-1.9)`,
		`2026-01-02T00:06:48.703891+00:00 mailhost pmg-smtp-filter[1004]: C0FFEE0000000002: accept mail to <recipient@example.net> (B0B0B0B0B0) (rule: default-accept)`,
		`2026-01-02T00:06:48.800000+00:00 mailhost postfix/lmtp[1005]: A2A2A2A202: to=<recipient@example.net>, relay=127.0.0.1[127.0.0.1]:10024, dsn=2.5.0, status=sent (250 2.5.0 OK (C0FFEE0000000002))`,
		`2026-01-02T00:06:48.900000+00:00 mailhost postfix/qmgr[1006]: A2A2A2A202: removed`,
	}
	recs := parseLines(lines)
	r := recordByQID(recs, "A2A2A2A202")
	if r == nil {
		t.Fatal("no record for queue id A2A2A2A202")
	}
	if r.Disposition != "sent" {
		t.Errorf("Disposition = %q, want %q", r.Disposition, "sent")
	}
	if r.FilterAction != "accept" {
		t.Errorf("FilterAction = %q, want %q", r.FilterAction, "accept")
	}
	if r.SpamScore != "0/5" {
		t.Errorf("SpamScore = %q, want %q", r.SpamScore, "0/5")
	}
}

// TestMilterRejectSpam: an inline milter reject ("Blocked by SpamAssassin") should be
// classified as spam, with no content-filter correlation needed.
func TestMilterRejectSpam(t *testing.T) {
	lines := []string{
		`2026-01-02T02:34:16.250609+00:00 mailhost postfix/cleanup[1007]: A3A3A3A303: milter-reject: END-OF-MESSAGE from mail.example.org[192.0.2.20]: 5.7.1 Blocked by SpamAssassin; from=<spammer@example.org> to=<recipient@example.net> proto=ESMTP helo=<mail.example.org>`,
	}
	recs := parseLines(lines)
	r := recordByQID(recs, "A3A3A3A303")
	if r == nil {
		t.Fatal("no record for queue id A3A3A3A303")
	}
	if r.Status != "reject" {
		t.Errorf("Status = %q, want %q", r.Status, "reject")
	}
	if r.Disposition != "spam" {
		t.Errorf("Disposition = %q, want %q", r.Disposition, "spam")
	}
	if r.From != "spammer@example.org" {
		t.Errorf("From = %q", r.From)
	}
}

// TestNoQueueReject: SMTP-time rejections (postscreen RBL block) must become records
// even though Postfix never assigned them a queue id.
func TestNoQueueReject(t *testing.T) {
	line := `2026-01-02T00:03:29.430062+00:00 mailhost postfix/postscreen[1008]: NOQUEUE: reject: RCPT from [192.0.2.30]:48596: 550 5.7.1 Service unavailable; client [192.0.2.30] blocked using rbl.example.net; from=<spammer@example.org>, to=<recipient@example.net>, proto=ESMTP, helo=<mail.example.org>`
	recs := parseLines([]string{line})
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Disposition != "rejected" {
		t.Errorf("Disposition = %q, want %q", r.Disposition, "rejected")
	}
	if r.FilterAction != "noqueue-reject" {
		t.Errorf("FilterAction = %q, want %q", r.FilterAction, "noqueue-reject")
	}
	if r.From != "spammer@example.org" {
		t.Errorf("From = %q", r.From)
	}
	if r.To != "recipient@example.net" {
		t.Errorf("To = %q", r.To)
	}
	if r.Client != "[192.0.2.30]" {
		t.Errorf("Client = %q, want %q", r.Client, "[192.0.2.30]")
	}
	// Deterministic synthetic id: same line parses to the same id (dedupe on re-parse).
	again := parseLines([]string{line})
	if again[0].QueueID != r.QueueID {
		t.Errorf("synthetic id not stable: %q vs %q", again[0].QueueID, r.QueueID)
	}
}

// TestNoQueue4xxStillRejected: postfix logs some policy rejections with a 4xx code
// (e.g. "Sender address rejected: Domain not found") but they are rejections, not
// deferrals — a NOQUEUE: reject is always classified as rejected.
func TestNoQueue4xxStillRejected(t *testing.T) {
	line := `2026-01-02T02:34:28.293011+00:00 mailhost postfix/smtpd[1009]: NOQUEUE: reject: RCPT from mail.example.org[192.0.2.40]: 450 4.1.8 <spammer@example.org>: Sender address rejected: Domain not found; from=<spammer@example.org> to=<recipient@example.net> proto=ESMTP helo=<mail.example.org>`
	recs := parseLines([]string{line})
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Disposition != "rejected" {
		t.Errorf("Disposition = %q, want %q", recs[0].Disposition, "rejected")
	}
}

// TestDeliveryLegMerge: a scanned+accepted message has two postfix queue ids (the
// inbound leg to 127.0.0.1 and the re-injected outbound leg to the real MX). They
// must collapse into one item that carries the inbound metadata + the real
// destination relay and final status.
func TestDeliveryLegMerge(t *testing.T) {
	lines := []string{
		// inbound leg -> local scanner
		`2026-01-02T04:13:31.454917+00:00 mailhost postfix/smtpd[2001]: connect from client.example.org[192.0.2.50]`,
		`2026-01-02T04:13:32.340938+00:00 mailhost postfix/smtpd[2001]: A1A1A1A101: client=client.example.org[192.0.2.50]`,
		`2026-01-02T04:13:32.469279+00:00 mailhost postfix/cleanup[2002]: A1A1A1A101: warning: header Subject: Quarterly Report from client.example.org[192.0.2.50]; from=<sender@example.com> to=<recipient@example.net> proto=ESMTP helo=<client.example.org>`,
		`2026-01-02T04:13:32.469775+00:00 mailhost postfix/cleanup[2002]: A1A1A1A101: message-id=<msg010@example.com>`,
		`2026-01-02T04:13:35.744897+00:00 mailhost postfix/qmgr[2003]: A1A1A1A101: from=<sender@example.com>, size=12921, nrcpt=1 (queue active)`,
		`2026-01-02T04:13:36.540458+00:00 mailhost postfix/lmtp[2004]: A1A1A1A101: to=<recipient@example.net>, relay=127.0.0.1[127.0.0.1]:10024, dsn=2.5.0, status=sent (250 2.5.0 OK (C0FFEE0000000010))`,
		`2026-01-02T04:13:36.541521+00:00 mailhost postfix/qmgr[2003]: A1A1A1A101: removed`,
		// scanner verdict (accept) -> re-injects under new queue id B1B1B1B1B1
		`2026-01-02T04:13:31.900000+00:00 mailhost pmg-smtp-filter[2010]: C0FFEE0000000010: new mail message-id=<msg010@example.com>`,
		`2026-01-02T04:13:36.100000+00:00 mailhost pmg-smtp-filter[2010]: C0FFEE0000000010: SA score=0/5 time=0.5 hits=BAYES_00(-1.9)`,
		`2026-01-02T04:13:36.400000+00:00 mailhost pmg-smtp-filter[2010]: C0FFEE0000000010: accept mail to <recipient@example.net> (B1B1B1B1B1) (rule: default-accept)`,
		`2026-01-02T04:13:36.450000+00:00 mailhost pmg-smtp-filter[2010]: C0FFEE0000000010: processing time: 0.5 seconds`,
		// outbound leg -> real destination
		`2026-01-02T04:13:36.478149+00:00 mailhost postfix/smtpd[2020]: connect from localhost.localdomain[127.0.0.1]`,
		`2026-01-02T04:13:36.482637+00:00 mailhost postfix/smtpd[2020]: B1B1B1B1B1: client=localhost.localdomain[127.0.0.1], orig_client=client.example.org[192.0.2.50]`,
		`2026-01-02T04:13:36.484570+00:00 mailhost postfix/cleanup[2002]: B1B1B1B1B1: message-id=<msg010@example.com>`,
		`2026-01-02T04:13:36.529624+00:00 mailhost postfix/qmgr[2003]: B1B1B1B1B1: from=<sender@example.com>, size=14643, nrcpt=1 (queue active)`,
		`2026-01-02T04:13:36.718329+00:00 mailhost postfix/smtp[2021]: B1B1B1B1B1: to=<recipient@example.net>, relay=mx.example.net[203.0.113.10]:25, dsn=2.6.0, status=sent (250 2.6.0 <msg010@example.com> [InternalId=12345] 16012 bytes Queued mail for delivery)`,
		`2026-01-02T04:13:36.718654+00:00 mailhost postfix/qmgr[2003]: B1B1B1B1B1: removed`,
	}

	store := NewStore(nil)
	store.AddRecords(parseLines(lines))

	st := store.Stats(SearchParams{})
	if st.Total != 1 {
		t.Fatalf("Stats.Total = %d, want 1 (legs should merge)", st.Total)
	}
	if st.Sent != 1 {
		t.Errorf("Stats.Sent = %d, want 1", st.Sent)
	}

	d := store.GetDetail("", "A1A1A1A101")
	if d == nil {
		t.Fatal("no merged record for inbound id A1A1A1A101")
	}
	if d.Relay != "mx.example.net[203.0.113.10]:25" {
		t.Errorf("Relay = %q, want the real destination", d.Relay)
	}
	if d.Disposition != "sent" {
		t.Errorf("Disposition = %q, want sent", d.Disposition)
	}
	if d.DeliveryQueueID != "B1B1B1B1B1" {
		t.Errorf("DeliveryQueueID = %q, want B1B1B1B1B1", d.DeliveryQueueID)
	}
	if d.Subject != "Quarterly Report" {
		t.Errorf("Subject = %q, want Quarterly Report (from inbound leg)", d.Subject)
	}
	if d.FilterAction != "accept" {
		t.Errorf("FilterAction = %q, want accept", d.FilterAction)
	}
	if !strings.Contains(d.Client, "192.0.2.50") {
		t.Errorf("Client = %q, want the real external origin", d.Client)
	}

	// The outbound queue id resolves to the same merged item.
	if d2 := store.GetDetail("", "B1B1B1B1B1"); d2 == nil || d2.QueueID != "A1A1A1A101" {
		t.Fatalf("outbound id did not resolve to the merged primary: %+v", d2)
	}

	// The merged timeline includes both postfix legs plus the scanner lines.
	if len(d.Lines) < 13 {
		t.Errorf("merged record has %d log lines, want >= 13", len(d.Lines))
	}
}

// TestRspamdCorrelation: an rspamd verdict (from rspamd.log) must attach to the
// matching postfix mail record by queue id — enriching it, never creating a row.
func TestRspamdCorrelation(t *testing.T) {
	mailLines := []string{
		`2026-01-02T01:51:31.353736+00:00 mailhost postfix/smtpd[3001]: D1D1D1D101: client=client.example.org[198.51.100.10]`,
		`2026-01-02T01:51:31.355933+00:00 mailhost postfix/cleanup[3002]: D1D1D1D101: message-id=<msg020@example.com>`,
		`2026-01-02T01:51:32.430937+00:00 mailhost postfix/qmgr[3003]: D1D1D1D101: from=<sender@example.com>, size=811921, nrcpt=2 (queue active)`,
		`2026-01-02T01:51:46.319289+00:00 mailhost postfix/smtp[3004]: D1D1D1D101: to=<recipient@example.net>, relay=mx.example.net[203.0.113.20]:25, dsn=2.0.0, status=sent (250 2.0.0 Ok: queued as ABCXYZ)`,
		`2026-01-02T01:51:46.320332+00:00 mailhost postfix/qmgr[3003]: D1D1D1D101: removed`,
	}
	store := NewStore(nil)
	store.AddRecords(parseLines(mailLines))

	rspamdLine := `2026-01-02 01:51:32 #2934(normal) <abc123>; task; rspamd_task_write_log: id: <msg020@example.com>, qid: <D1D1D1D101>, ip: 198.51.100.10, from: <sender@example.com>, (default: F (no action): [7.83/nan] [MISSING_TO(2.00){}]), len: 811921, time: 100ms, rcpts: <recipient@example.net>`
	qid, v, ok := parseRspamdLine(rspamdLine)
	if !ok {
		t.Fatal("parseRspamdLine failed to parse a task line")
	}
	if qid != "D1D1D1D101" {
		t.Fatalf("qid = %q, want D1D1D1D101", qid)
	}
	if matched := store.ApplyRspamdVerdict(qid, v); !matched {
		t.Fatal("rspamd verdict did not correlate to the mail record")
	}

	// Still exactly one item (enriched, not duplicated).
	if st := store.Stats(SearchParams{}); st.Total != 1 {
		t.Fatalf("Stats.Total = %d, want 1 (rspamd must not create rows)", st.Total)
	}
	d := store.GetDetail("", "D1D1D1D101")
	if d == nil {
		t.Fatal("no record for D1D1D1D101")
	}
	if d.Filter != "rspamd" {
		t.Errorf("Filter = %q, want rspamd", d.Filter)
	}
	if d.FilterAction != "no action" {
		t.Errorf("FilterAction = %q, want 'no action'", d.FilterAction)
	}
	if d.SpamScore != "7.83" {
		t.Errorf("SpamScore = %q, want 7.83", d.SpamScore)
	}
	if d.Disposition != "sent" {
		t.Errorf("Disposition = %q, want sent (no action keeps it delivered)", d.Disposition)
	}
}

// TestRspamdPendingThenRecord: a verdict arriving before its mail record (live mode)
// is held and applied when the record is added. "add header" tags the message but it is
// still DELIVERED, so the disposition stays "sent" — the score/action ride along.
func TestRspamdPendingThenRecord(t *testing.T) {
	store := NewStore(nil)
	line := `2026-01-02 02:00:00 #1(normal) <def456>; task; rspamd_task_write_log: id: <msg030@example.org>, qid: <E1E1E1E101>, ip: 192.0.2.60, from: <spammer@example.org>, (default: T (add header): [9.50/15.00] [BAYES_SPAM(5.0){}]), len: 10, time: 1ms, rcpts: <recipient@example.net>`
	qid, v, _ := parseRspamdLine(line)
	if store.ApplyRspamdVerdict(qid, v) {
		t.Fatal("should not match before the record exists")
	}
	// now the mail record arrives
	store.AddRecords(parseLines([]string{
		`2026-01-02T12:00:00.0+00:00 mailhost postfix/smtpd[1]: E1E1E1E101: client=client.example.org[192.0.2.60]`,
		`2026-01-02T12:00:01.0+00:00 mailhost postfix/smtp[1]: E1E1E1E101: to=<recipient@example.net>, relay=mx.example.net[203.0.113.30]:25, dsn=2.0.0, status=sent (250 ok)`,
		`2026-01-02T12:00:01.0+00:00 mailhost postfix/qmgr[1]: E1E1E1E101: removed`,
	}))
	d := store.GetDetail("", "E1E1E1E101")
	if d == nil || d.FilterAction != "add header" {
		t.Fatalf("pending verdict not applied on record add: %+v", d)
	}
	if d.Disposition != "sent" {
		t.Errorf("Disposition = %q, want sent (add header tags but the mail is still delivered)", d.Disposition)
	}
	if d.SpamScore != "9.50" {
		t.Errorf("SpamScore = %q, want 9.50 (score rides along on the delivered record)", d.SpamScore)
	}
}

// TestOriginIdentity: in distributed mode two servers can emit the same Postfix
// queue id. Records must be keyed by (origin, queue id) so they don't collide, and
// each must be independently retrievable. Standalone (empty origin) is unaffected.
func TestOriginIdentity(t *testing.T) {
	store := NewStore(nil)
	store.AddRecords([]Record{
		{Origin: "node-a", QueueID: "ABCDEF0123", Status: "sent", Disposition: "sent"},
		{Origin: "node-b", QueueID: "ABCDEF0123", Status: "deferred", Disposition: "deferred"},
	})
	if st := store.Stats(SearchParams{}); st.Total != 2 {
		t.Fatalf("Stats.Total = %d, want 2 (same qid, different origin must not collide)", st.Total)
	}
	if !store.HasOrigins() {
		t.Error("HasOrigins() = false, want true once a record carries an origin")
	}
	if d := store.GetDetail("node-a", "ABCDEF0123"); d == nil || d.Origin != "node-a" || d.Disposition != "sent" {
		t.Fatalf("node-a detail wrong: %+v", d)
	}
	if d := store.GetDetail("node-b", "ABCDEF0123"); d == nil || d.Origin != "node-b" || d.Disposition != "deferred" {
		t.Fatalf("node-b detail wrong: %+v", d)
	}
	if store.GetDetail("", "ABCDEF0123") != nil {
		t.Error("empty-origin lookup must not resolve an origin-scoped record")
	}
}

// TestNormalSentUnaffected: ordinary relay delivery (no local filter) stays "sent".
func TestNormalSentUnaffected(t *testing.T) {
	lines := []string{
		`2026-01-02T01:51:46.319289+00:00 mailhost postfix/smtp[4001]: F1F1F1F101: to=<recipient@example.net>, relay=mx.example.net[203.0.113.40]:25, dsn=2.0.0, status=sent (250 2.0.0 Ok: queued as QUEUEDID)`,
		`2026-01-02T01:51:46.400000+00:00 mailhost postfix/qmgr[4002]: F1F1F1F101: removed`,
	}
	recs := parseLines(lines)
	r := recordByQID(recs, "F1F1F1F101")
	if r == nil {
		t.Fatal("no record for F1F1F1F101")
	}
	if r.Disposition != "sent" {
		t.Errorf("Disposition = %q, want %q", r.Disposition, "sent")
	}
	if r.FilterAction != "" {
		t.Errorf("unexpected FilterAction = %q", r.FilterAction)
	}
}

// TestSubjectLocalSubmission: header_checks logs "Subject: … from local;" for locally
// submitted mail (no host[ip]); the subject must still be extracted.
func TestSubjectLocalSubmission(t *testing.T) {
	lines := []string{
		`2026-01-02T05:30:36.912883+10:00 host postfix/pickup[1]: D0D0D0D001: uid=0 from=<support@example.com>`,
		`2026-01-02T05:30:36.923953+10:00 host postfix/cleanup[2]: D0D0D0D001: warning: header Subject: [DOWNTIME] reboot at 2:30AM tomorrow from local; from=<support@example.com>`,
		`2026-01-02T05:30:37.643252+10:00 host postfix/qmgr[3]: D0D0D0D001: from=<support@example.com>, size=9477, nrcpt=1 (queue active)`,
		`2026-01-02T05:30:39.293023+10:00 host postfix/local[4]: D0D0D0D001: to=<support@example.com>, relay=local, dsn=2.0.0, status=sent (delivered to command: /usr/bin/x)`,
		`2026-01-02T05:30:44.311212+10:00 host postfix/qmgr[3]: D0D0D0D001: removed`,
	}
	recs := parseLines(lines)
	r := recordByQID(recs, "D0D0D0D001")
	if r == nil {
		t.Fatal("no record for D0D0D0D001")
	}
	if r.Subject != "[DOWNTIME] reboot at 2:30AM tomorrow" {
		t.Errorf("Subject = %q, want %q", r.Subject, "[DOWNTIME] reboot at 2:30AM tomorrow")
	}
}

// TestFilterBlockRule: a pmg-smtp-filter "block mail to … (rule: …)" line must capture
// the rule into the verdict (it previously only set the action/disposition).
func TestFilterBlockRule(t *testing.T) {
	line := `2026-01-02T20:28:54.0+10:00 gw pmg-smtp-filter[99]: ABCDEF01234567: block mail to <dl@example.com> (rule: Block stupid domains)`
	fid, v, ok := parseFilterLine(line)
	if !ok {
		t.Fatal("parseFilterLine ok=false")
	}
	if fid != "ABCDEF01234567" {
		t.Errorf("fid = %q, want ABCDEF01234567", fid)
	}
	if v.action != "block" || v.disposition != "blocked" {
		t.Errorf("action=%q disposition=%q, want block/blocked", v.action, v.disposition)
	}
	if v.rule != "Block stupid domains" {
		t.Errorf("rule = %q, want 'Block stupid domains'", v.rule)
	}
}
