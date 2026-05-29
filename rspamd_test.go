package main

import (
	"testing"
	"time"
)

// TestExtractRspamdTimeLocal: rspamd's zone-less timestamp must be read in the host's
// local zone (matching Postfix's RFC3339 instants), not as UTC — otherwise rspamd lines
// skew by the UTC offset in the timeline.
func TestExtractRspamdTimeLocal(t *testing.T) {
	got := extractRspamdTime("2026-05-24 14:37:13 #1(normal) <x>; task; rspamd_task_write_log: ...")
	want := time.Date(2026, 5, 24, 14, 37, 13, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("extractRspamdTime = %v, want %v (local wall-clock)", got, want)
	}
}

// TestRspamdDisposition pins the action→disposition mapping. Only "reject" actually
// stops the mail; "add header"/"rewrite subject"/"no action" tag-or-pass and leave the
// postfix disposition untouched (the message is still delivered); soft reject/greylist
// defer. Regression for the "spam yet delivered to mailbox" bug.
func TestRspamdDisposition(t *testing.T) {
	cases := map[string]string{
		"reject":          "spam",
		"add header":      "", // tagged, still delivered → no override
		"rewrite subject": "", // subject tagged, still delivered → no override
		"no action":       "",
		"soft reject":     "deferred",
		"greylist":        "deferred",
		"":                "",
		"unknown action":  "",
	}
	for action, want := range cases {
		if got := rspamdDisposition(action); got != want {
			t.Errorf("rspamdDisposition(%q) = %q, want %q", action, got, want)
		}
	}
}

// TestRspamdEnrichKeepsDeliveredOnTag: a delivered message that rspamd merely tagged
// must stay delivered, with the score and action attached so the flagging is still
// visible. This is the exact shape of a real add-header case (score 6.83,
// then a local mailbox delivery).
func TestRspamdEnrichKeepsDeliveredOnTag(t *testing.T) {
	rec := &Record{
		Disposition:  "delivered",
		Status:       "sent",
		StatusDetail: "(250 2.0.0 <backup@example.net> Saved)",
		Relay:        "mail.example.net[private/dovecot-lmtp]",
	}
	enrichRecordWithRspamd(rec, rspamdVerdict{action: "add header", score: "6.83"})
	if rec.Disposition != "delivered" {
		t.Errorf("Disposition = %q, want delivered (add header does not block)", rec.Disposition)
	}
	if rec.SpamScore != "6.83" {
		t.Errorf("SpamScore = %q, want 6.83 (score must still ride along)", rec.SpamScore)
	}
	if rec.FilterAction != "add header" || rec.Filter != "rspamd" {
		t.Errorf("filter not attached: action=%q filter=%q", rec.FilterAction, rec.Filter)
	}
}

// TestRspamdEnrichRejectBlocks: a true reject is spam (the message was stopped), so it
// overrides the postfix disposition.
func TestRspamdEnrichRejectBlocks(t *testing.T) {
	rec := &Record{Disposition: "sent", Status: "sent"}
	enrichRecordWithRspamd(rec, rspamdVerdict{action: "reject", score: "15.5"})
	if rec.Disposition != "spam" {
		t.Errorf("Disposition = %q, want spam (reject blocks the mail)", rec.Disposition)
	}
}

// TestParseRspamdAddHeaderLine: the task-summary parser pulls qid, action, score and id
// from a real-shaped "add header" line.
func TestParseRspamdAddHeaderLine(t *testing.T) {
	line := `2026-05-28 11:31:19 #4242(normal) <a1b2c3>; task; rspamd_task_write_log: id: <msg-add@example.net>, qid: <A1B2C3D4E5>, ip: 198.51.100.18, from: <sender@example.net>, (default: T (add header): [6.83/15.00] [BAYES_SPAM(5.0){}]), len: 188249, time: 100ms, rcpts: <recipient@example.org>`
	qid, v, ok := parseRspamdLine(line)
	if !ok {
		t.Fatal("parseRspamdLine returned ok=false")
	}
	if qid != "A1B2C3D4E5" {
		t.Errorf("qid = %q, want A1B2C3D4E5", qid)
	}
	if v.action != "add header" {
		t.Errorf("action = %q, want 'add header'", v.action)
	}
	if v.score != "6.83" {
		t.Errorf("score = %q, want 6.83", v.score)
	}
	if v.messageID != "msg-add@example.net" {
		t.Errorf("messageID = %q, want msg-add@example.net", v.messageID)
	}
}

// TestRspamdTimelineTZ: an rspamd line is zone-less, so its timeline timestamp must be
// resolved in the SAME offset as the record's Postfix lines (which carry it in their
// RFC3339 text) — not as UTC. This holds even when Record.Timestamp has been normalised
// to UTC, as it is on a distributed receiver after forwarding. Regression for rspamd
// entries skewing by the host offset in the timeline.
func TestRspamdTimelineTZ(t *testing.T) {
	store := NewStore(nil)
	store.records = []Record{{
		QueueID:   "ABCDEF0123",
		Timestamp: time.Date(2026, 5, 28, 5, 53, 50, 0, time.UTC), // normalised, as on the receiver
		RawLines: []string{
			`2026-05-28T15:53:50.000000+10:00 node-a postfix/qmgr[1]: ABCDEF0123: removed`,
			`2026-05-28 15:53:50 #1(normal) <x>; task; rspamd_task_write_log: id: <m@example.net>, qid: <ABCDEF0123>, (default: F (no action): [0.0/15.0])`,
		},
	}}
	store.byQueueID = map[string]int{recKey("", "ABCDEF0123"): 0}

	d := store.GetDetail("", "ABCDEF0123")
	if d == nil || len(d.Lines) != 2 {
		t.Fatalf("expected 2 timeline lines, got %+v", d)
	}
	// Both the Postfix and the rspamd line are the same wall-clock (15:53:50 +10:00),
	// so both must resolve to 05:53:50Z.
	for _, l := range d.Lines {
		if l.Timestamp != "2026-05-28T05:53:50Z" {
			t.Errorf("line ts = %q, want 2026-05-28T05:53:50Z (raw: %.50s)", l.Timestamp, l.Raw)
		}
	}
}
