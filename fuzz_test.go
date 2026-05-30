package main

import (
	"strings"
	"testing"
)

// FuzzParseLines exercises the pure log parser with arbitrary input. parseLines is the
// untrusted-input boundary — it consumes raw mail.log lines (and gzipped rotations) — so
// it must be total: never panic, index out of range, or hang, no matter how malformed the
// line. The fuzzer explores past the structured seeds toward adversarial shapes (truncated
// queue ids, missing fields, control bytes, huge tokens). Run: go test -run=Fuzz -fuzz=FuzzParseLines
func FuzzParseLines(f *testing.F) {
	seeds := []string{
		`2026-05-28T15:53:50.000000+10:00 host postfix/smtpd[1]: ABCDEF0123: client=mx.sender.example[198.51.100.10]`,
		`2026-05-28T15:53:50.000000+10:00 host postfix/cleanup[1]: ABCDEF0123: message-id=<m@example.net>`,
		`2026-05-28T15:53:50.000000+10:00 host postfix/qmgr[1]: ABCDEF0123: from=<a@example.net>, size=2048, nrcpt=1 (queue active)`,
		`2026-05-28T15:53:50.000000+10:00 host postfix/smtp[1]: ABCDEF0123: to=<b@example.org>, relay=mx.example.org[203.0.113.5]:25, status=sent (250 OK)`,
		`2026-05-28T15:53:50.000000+10:00 host postfix/smtpd[1]: NOQUEUE: reject: RCPT from x[198.51.100.9]: 554 5.7.1 blocked; from=<a@example.net> to=<b@example.org>`,
		`2026-05-28 15:53:50 #1(normal) <x>; task; rspamd_task_write_log: id: <m@example.net>, qid: <ABCDEF0123>, (default: F (no action): [0.0/15.0])`,
		`ABCDEF0123: removed`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		// A single line and a couple of lines together (queue-id grouping path) — both
		// must return without panicking.
		_ = parseLines([]string{line})
		_ = parseLines(strings.Split(line, "\n"))
	})
}
