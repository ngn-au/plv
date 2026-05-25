// Command loggen generates synthetic Proxmox Mail Gateway (PMG) style Postfix
// logs for screenshots and local testing of PLV.
//
// It writes ONLY to the output directory (default ./logs/sample) and never reads
// any existing logs. All data is fictional: example.com/.net/.org and the
// `.example` documentation TLD (RFC 2606), plus RFC 5737 documentation IP ranges
// (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24).
//
// The log shapes are derived from PLV's parser (parser.go), not from any real
// log: queue-id grouping, the pmg-smtp-filter handoff line that carries the
// filter session id (`status=sent (250 2.5.0 OK (<sid>))`), the filter verdict
// lines (SA score / quarantine / accept / block), the inbound→outbound leg merge
// via the `accept mail … (<qid>)` onward queue id, and standalone NOQUEUE
// rejections. Output is split across rotated files (mail.log, mail.log.1, and
// gzipped mail.log.2.gz…mail.log.4.gz) to exercise discovery + gzip handling.
//
//	go run ./tools/loggen                                   # 30 days → ./logs/sample
//	go run ./tools/loggen -days 14 -per-day 1500 -seed 42   # custom volume
//	go run ./tools/loggen -out /tmp/plvdata                 # custom output dir
package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	tsLayout = "2006-01-02T15:04:05.000000Z07:00" // RFC3339 with microseconds
	hostname = "mailgw"                           // the PMG host (syslog hostname field)
	spamThr  = 5                                  // PMG default spam threshold (SA score / 5)
)

// rotated log files, newest-first by age bucket; older buckets are gzipped, just
// like logrotate keeps recent rotations plain and compresses the rest.
var files = []struct {
	name    string
	gzip    bool
	maxDays int // upper bound (inclusive) of message age, in days, for this file
}{
	{"mail.log", false, 6},
	{"mail.log.1", false, 13},
	{"mail.log.2.gz", true, 20},
	{"mail.log.3.gz", true, 27},
	{"mail.log.4.gz", true, 100000},
}

// ---- synthetic data pools (documentation-safe only) ------------------------

var (
	localDomains = []string{"example.com", "corp.example", "team.example.org"}
	localUsers   = []string{
		"alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi",
		"support", "sales", "finance", "hr", "info", "accounts", "no-reply", "helpdesk",
	}

	legitSenderDomains = []string{
		"news.example.org", "billing.acme.example", "shop.widgets.example",
		"newsletter.example.net", "notifications.example.net", "mail.partner.example",
		"updates.vendor.example", "noreply.bank.example", "team.project.example",
	}
	legitSenderUsers = []string{
		"newsletter", "billing", "orders", "notifications", "no-reply", "support",
		"updates", "alerts", "statements", "team", "ci",
	}

	spamSenderDomains = []string{
		"promo.deals.example", "winner.lottery.example", "offers.junk.example",
		"pharma.cheap.example", "secure-verify.example", "crypto.invest.example",
	}
	spamSenderUsers = []string{"win", "promo", "offer", "verify", "account", "no-reply", "deals", "vip"}

	mailstores = []struct {
		host string
		ip   string
		port int
	}{
		{"mailstore.example.com", "192.0.2.20", 25},
		{"imap.example.com", "192.0.2.21", 24},
		{"exchange.corp.example", "192.0.2.22", 25},
	}

	legitSubjects = []string{
		"Q2 budget review", "Lunch tomorrow?", "Re: project update", "Meeting notes",
		"Your order has shipped", "Invoice 4021 ready", "Monthly statement", "Welcome aboard",
		"Password reset requested", "Weekly digest", "Re: contract draft", "Standup at 9",
		"Deployment complete", "Action required: renew subscription", "Photos from the trip",
		"Re: question about pricing", "Onboarding checklist", "Server maintenance window",
	}
	spamSubjects = []string{
		"You won a prize!!!", "Cheap meds online now", "Verify your account immediately",
		"Hot investment opportunity", "Claim your reward today", "Final notice: account suspended",
		"Congratulations, you are selected", "Limited time crypto offer", "URGENT: confirm payment",
		"Increase your followers fast",
	}

	spamRules = []string{
		"Quarantine/Mark Spam (Level 3)", "Quarantine/Mark Spam (Level 4)",
		"Quarantine/Mark Spam (Level 5)",
	}
	virusRules  = []string{"Virus (Eicar-Test-Signature)", "Virus (Trojan.Generic.123)", "Virus (Worm.Mydoom.A)"}
	blockRules  = []string{"Blacklist", "Block Dangerous Attachment (.exe)", "Block Dangerous Attachment (.js)"}
	acceptRules = []string{"default-accept", "Whitelist", "default-accept", "default-accept"}

	saHamHits  = []string{"BAYES_00", "DKIM_VALID,SPF_PASS", "HTML_MESSAGE", "BAYES_05,DKIM_VALID"}
	saSpamHits = []string{
		"BAYES_99,HTML_IMAGE_ONLY,URIBL_BLACK", "BAYES_999,FROM_SUSPICIOUS_NTLD,HTML_MESSAGE",
		"BAYES_99,RAZOR2_CHECK,DCC_CHECK,URIBL_DBL_SPAM",
	}
)

// message kinds and their relative weights.
type kind int

const (
	kindDelivered kind = iota
	kindSpam
	kindNoQueue
	kindBounced
	kindDeferred
	kindVirus
	kindBlocked
)

var kindWeights = []struct {
	k kind
	w int
}{
	{kindDelivered, 64},
	{kindSpam, 16},
	{kindNoQueue, 8},
	{kindBounced, 4},
	{kindDeferred, 3},
	{kindVirus, 2},
	{kindBlocked, 3},
}

// hourly weights produce a diurnal pattern (quiet overnight, busy 8:00–18:00).
var hourWeights = [24]int{
	2, 1, 1, 1, 1, 2, 4, 8, 14, 18, 20, 19, 16, 18, 20, 19, 16, 12, 9, 7, 6, 5, 4, 3,
}

type entry struct {
	ts   time.Time
	text string
}

type gen struct {
	r       *rand.Rand
	loc     *time.Location
	now     time.Time
	buckets [][]entry
	counts  map[kind]int
}

func main() {
	days := flag.Int("days", 30, "number of days of history to generate")
	perDay := flag.Int("per-day", 800, "approximate messages per weekday (weekends are lighter)")
	out := flag.String("out", "logs/sample", "output directory (created if missing; never read)")
	seed := flag.Uint64("seed", 1, "PRNG seed for reproducible output")
	tz := flag.String("tz", "UTC", "timezone for log timestamps (e.g. UTC, Australia/Sydney)")
	flag.Parse()

	loc, err := time.LoadLocation(*tz)
	if err != nil {
		log.Fatalf("invalid -tz %q: %v", *tz, err)
	}

	g := &gen{
		r:       rand.New(rand.NewPCG(*seed, 0x9E3779B97F4A7C15)),
		loc:     loc,
		now:     time.Now().In(loc),
		buckets: make([][]entry, len(files)),
		counts:  make(map[kind]int),
	}

	start := g.now.AddDate(0, 0, -*days)
	for d := 0; d < *days; d++ {
		day := start.AddDate(0, 0, d)
		factor := 1.0
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			factor = 0.45 // lighter weekend traffic
		}
		count := int(float64(*perDay) * factor * (0.8 + 0.4*g.r.Float64()))
		for i := 0; i < count; i++ {
			ts := time.Date(day.Year(), day.Month(), day.Day(), g.weightedHour(),
				g.r.IntN(60), g.r.IntN(60), g.r.IntN(1_000_000)*1000, g.loc)
			if ts.After(g.now) {
				continue // don't emit future-dated lines
			}
			g.message(ts)
		}
	}

	if err := g.write(*out); err != nil {
		log.Fatalf("write: %v", err)
	}
	g.summary(*out, start)
}

// ---- message construction --------------------------------------------------

func (g *gen) message(ts time.Time) {
	k := g.pickKind()
	g.counts[k]++
	fileIdx := g.fileFor(ts)
	cur := ts

	add := func(text string) {
		g.buckets[fileIdx] = append(g.buckets[fileIdx], entry{cur, cur.Format(tsLayout) + " " + hostname + " " + text})
		cur = cur.Add(time.Duration(40+g.r.IntN(900)) * time.Millisecond)
	}

	if k == kindNoQueue {
		g.noQueueReject(add)
		return
	}

	// Common inbound flow: external client → PMG smtpd → pmg-smtp-filter (after
	// queue, via lmtp on :10024) → verdict.
	q1 := g.queueID()
	sid := g.hex(16)
	from, fromDomain := g.sender(k == kindSpam || k == kindVirus || k == kindBlocked)
	to := g.recipient()
	subject := g.subject(k)
	size := 1024 + g.r.IntN(900_000)
	msgID := fmt.Sprintf("<%s.%s@%s>", ts.Format("20060102150405"), g.hex(8), fromDomain)
	clientHost, clientIP := g.client(fromDomain, k)
	smtpdPID, cleanupPID, qmgrPID := g.pid(), g.pid(), g.pid()
	lmtpPID, filterPID := g.pid(), g.pid()

	add(fmt.Sprintf("postfix/smtpd[%d]: connect from %s[%s]", smtpdPID, clientHost, clientIP))
	add(fmt.Sprintf("postfix/smtpd[%d]: %s: client=%s[%s]", smtpdPID, q1, clientHost, clientIP))
	add(fmt.Sprintf("postfix/cleanup[%d]: %s: message-id=%s", cleanupPID, q1, msgID))
	if g.r.Float64() < 0.9 { // subject logging (header_checks) enabled most of the time
		add(fmt.Sprintf("postfix/cleanup[%d]: %s: warning: header Subject: %s from %s[%s]; from=<%s> to=<%s> proto=ESMTP",
			cleanupPID, q1, subject, clientHost, clientIP, from, to))
	}
	add(fmt.Sprintf("postfix/qmgr[%d]: %s: from=<%s>, size=%d, nrcpt=1 (queue active)", qmgrPID, q1, from, size))

	// pmg-smtp-filter session: a "new mail" line, an SA score, then the verdict.
	add(fmt.Sprintf("pmg-smtp-filter[%d]: %s: new mail message-id=%s", filterPID, sid, msgID))

	var onward string // onward (re-injected) queue id, set when the filter accepts
	switch k {
	case kindDelivered, kindBounced, kindDeferred:
		add(fmt.Sprintf("pmg-smtp-filter[%d]: %s: SA score=%s/%d time=%.3f bayes=%.2f hits=%s",
			filterPID, sid, g.score(0.1, 4.6), spamThr, 0.2+g.r.Float64(), g.r.Float64(), pick(g.r, saHamHits)))
		onward = g.queueID()
		add(fmt.Sprintf("pmg-smtp-filter[%d]: %s: accept mail to <%s> (%s) (rule: %s)",
			filterPID, sid, to, onward, pick(g.r, acceptRules)))
	case kindSpam:
		add(fmt.Sprintf("pmg-smtp-filter[%d]: %s: SA score=%s/%d time=%.3f bayes=%.2f hits=%s",
			filterPID, sid, g.score(5.0, 22.0), spamThr, 0.3+g.r.Float64(), 0.9+0.1*g.r.Float64(), pick(g.r, saSpamHits)))
		add(fmt.Sprintf("pmg-smtp-filter[%d]: %s: moved mail for <%s> to spam quarantine - %s (rule: %s)",
			filterPID, sid, to, g.hex(10), pick(g.r, spamRules)))
	case kindVirus:
		add(fmt.Sprintf("pmg-smtp-filter[%d]: %s: moved mail for <%s> to virus quarantine - %s (rule: %s)",
			filterPID, sid, to, g.hex(10), pick(g.r, virusRules)))
	case kindBlocked:
		add(fmt.Sprintf("pmg-smtp-filter[%d]: %s: block mail to <%s> (rule: %s)",
			filterPID, sid, to, pick(g.r, blockRules)))
	}

	// Optional TLS line for the visible (primary) record. Placed in the Q1 block
	// so it attaches to q1 (the parser carries TLS only on the leg it's seen on,
	// and the merge keeps the primary's TLS).
	if g.r.Float64() < 0.55 {
		ms := mailstores[g.r.IntN(len(mailstores))]
		add(fmt.Sprintf("postfix/smtp[%d]: Trusted TLS connection established to %s[%s]:%d: TLSv1.3 with cipher TLS_AES_256_GCM_SHA384 (256/256 bits) key-exchange X25519 server-signature ECDSA",
			g.pid(), ms.host, ms.ip, ms.port))
	}

	// The misleading hand-off: Postfix reports status=sent to the filter even when
	// the filter went on to quarantine/block. The (sid) links it to the verdict.
	add(fmt.Sprintf("postfix/lmtp[%d]: %s: to=<%s>, relay=127.0.0.1[127.0.0.1]:10024, delay=%.1f, dsn=2.5.0, status=sent (250 2.5.0 OK (%s))",
		lmtpPID, q1, to, 1.0+5*g.r.Float64(), sid))
	add(fmt.Sprintf("postfix/qmgr[%d]: %s: removed", qmgrPID, q1))

	// Outbound (re-injected) leg for accepted mail: real destination + final
	// delivery status. PLV merges this into the q1 record via the onward qid.
	if onward != "" {
		ms := mailstores[g.r.IntN(len(mailstores))]
		oQmgr, oSmtp := g.pid(), g.pid()
		add(fmt.Sprintf("postfix/qmgr[%d]: %s: from=<%s>, size=%d, nrcpt=1 (queue active)", oQmgr, onward, from, size))
		switch k {
		case kindDelivered:
			add(fmt.Sprintf("postfix/smtp[%d]: %s: to=<%s>, relay=%s[%s]:%d, delay=%.1f, delays=0.1/0/0.3/%.1f, dsn=2.0.0, status=sent (250 2.0.0 Ok: queued as %s)",
				oSmtp, onward, to, ms.host, ms.ip, ms.port, 0.5+3*g.r.Float64(), g.r.Float64(), g.queueID()))
			add(fmt.Sprintf("postfix/qmgr[%d]: %s: removed", oQmgr, onward))
		case kindBounced:
			add(fmt.Sprintf("postfix/smtp[%d]: %s: to=<%s>, relay=%s[%s]:%d, delay=%.1f, dsn=5.1.1, status=bounced (host %s[%s] said: 550 5.1.1 <%s>: Recipient address rejected: User unknown)",
				oSmtp, onward, to, ms.host, ms.ip, ms.port, 0.5+3*g.r.Float64(), ms.host, ms.ip, to))
			add(fmt.Sprintf("postfix/qmgr[%d]: %s: removed", oQmgr, onward))
		case kindDeferred:
			add(fmt.Sprintf("postfix/smtp[%d]: %s: to=<%s>, relay=%s[%s]:%d, delay=%.1f, dsn=4.4.1, status=deferred (connect to %s[%s]:%d: Connection timed out)",
				oSmtp, onward, to, ms.host, ms.ip, ms.port, 30+30*g.r.Float64(), ms.host, ms.ip, ms.port))
			// deferred mail stays queued — no "removed".
		}
	}
}

// noQueueReject emits a single standalone SMTP-time rejection (no queue id).
func (g *gen) noQueueReject(add func(string)) {
	host, ip := g.client(pick(g.r, spamSenderDomains), kindSpam)
	from, _ := g.sender(true)
	to := g.recipient()
	helo := host
	if g.r.Float64() < 0.5 {
		host = "unknown"
	}
	type rj struct {
		stage string
		resp  string
	}
	rejects := []rj{
		{"RCPT", fmt.Sprintf("554 5.7.1 Service unavailable; Client host [%s] blocked using zen.spamhaus.example", ip)},
		{"RCPT", fmt.Sprintf("550 5.1.1 <%s>: Recipient address rejected: User unknown in virtual mailbox table", to)},
		{"RCPT", fmt.Sprintf("450 4.1.8 <%s>: Sender address rejected: Domain not found", from)},
		{"RCPT", fmt.Sprintf("554 5.7.1 <%s>: Relay access denied", to)},
		{"MAIL", "451 4.7.1 Greylisted, please try again later"},
	}
	r := rejects[g.r.IntN(len(rejects))]
	add(fmt.Sprintf("postfix/smtpd[%d]: NOQUEUE: reject: %s from %s[%s]: %s; from=<%s> to=<%s> proto=ESMTP helo=<%s>",
		g.pid(), r.stage, host, ip, r.resp, from, to, helo))
}

// ---- output ----------------------------------------------------------------

func (g *gen) write(outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for i, f := range files {
		lines := g.buckets[i]
		sort.SliceStable(lines, func(a, b int) bool { return lines[a].ts.Before(lines[b].ts) })
		path := filepath.Join(outDir, f.name)
		if err := writeFile(path, f.gzip, lines); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func writeFile(path string, gz bool, lines []entry) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()

	var w interface {
		Write([]byte) (int, error)
	} = fh
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(fh)
		w = gzw
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteByte('\n')
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		return err
	}
	if gzw != nil {
		return gzw.Close()
	}
	return nil
}

func (g *gen) summary(outDir string, start time.Time) {
	total := 0
	for _, n := range g.counts {
		total += n
	}
	fmt.Printf("Generated %d messages (%s → %s, tz %s) into %s\n",
		total, start.Format("2006-01-02"), g.now.Format("2006-01-02"), g.loc, outDir)
	names := map[kind]string{
		kindDelivered: "delivered", kindSpam: "spam", kindVirus: "virus",
		kindBlocked: "blocked", kindBounced: "bounced", kindDeferred: "deferred", kindNoQueue: "noqueue-reject",
	}
	for _, kw := range kindWeights {
		fmt.Printf("  %-15s %6d\n", names[kw.k], g.counts[kw.k])
	}
	fmt.Println("Files:")
	for i, f := range files {
		var lo, hi time.Time
		for _, e := range g.buckets[i] {
			if lo.IsZero() || e.ts.Before(lo) {
				lo = e.ts
			}
			if e.ts.After(hi) {
				hi = e.ts
			}
		}
		span := "(empty)"
		if !lo.IsZero() {
			span = lo.Format("2006-01-02") + " … " + hi.Format("2006-01-02")
		}
		fmt.Printf("  %-14s %8d lines  %s\n", f.name, len(g.buckets[i]), span)
	}
	fmt.Printf("\nPoint PLV at it:  go run . -logdir %s   (or mount it read-only in the container)\n", outDir)
}

// ---- helpers ---------------------------------------------------------------

func (g *gen) pickKind() kind {
	total := 0
	for _, kw := range kindWeights {
		total += kw.w
	}
	n := g.r.IntN(total)
	for _, kw := range kindWeights {
		if n < kw.w {
			return kw.k
		}
		n -= kw.w
	}
	return kindDelivered
}

func (g *gen) weightedHour() int {
	total := 0
	for _, w := range hourWeights {
		total += w
	}
	n := g.r.IntN(total)
	for h, w := range hourWeights {
		if n < w {
			return h
		}
		n -= w
	}
	return 12
}

func (g *gen) fileFor(ts time.Time) int {
	age := int(g.now.Sub(ts).Hours() / 24)
	for i, f := range files {
		if age <= f.maxDays {
			return i
		}
	}
	return len(files) - 1
}

func (g *gen) sender(spam bool) (addr, domain string) {
	if spam {
		domain = pick(g.r, spamSenderDomains)
		return pick(g.r, spamSenderUsers) + "@" + domain, domain
	}
	domain = pick(g.r, legitSenderDomains)
	return pick(g.r, legitSenderUsers) + "@" + domain, domain
}

func (g *gen) recipient() string {
	return pick(g.r, localUsers) + "@" + pick(g.r, localDomains)
}

func (g *gen) subject(k kind) string {
	if k == kindSpam || k == kindVirus || k == kindBlocked {
		return pick(g.r, spamSubjects)
	}
	return pick(g.r, legitSubjects)
}

// client returns a plausible sending host + a documentation-range IP.
func (g *gen) client(domain string, k kind) (host, ip string) {
	ranges := []string{"192.0.2", "198.51.100", "203.0.113"}
	ip = fmt.Sprintf("%s.%d", ranges[g.r.IntN(len(ranges))], 1+g.r.IntN(253))
	if k == kindSpam || k == kindVirus || k == kindBlocked {
		if g.r.Float64() < 0.5 {
			return "unknown", ip
		}
	}
	return "mail." + domain, ip
}

func (g *gen) score(min, max float64) string {
	return fmt.Sprintf("%.1f", min+(max-min)*g.r.Float64())
}

func (g *gen) pid() int { return 600 + g.r.IntN(29000) }

// queueID returns an uppercase-hex Postfix queue id (10 chars), matching PLV's
// queueIDRe ([A-Fa-f0-9]{10,12}).
func (g *gen) queueID() string { return g.hex(10) }

func (g *gen) hex(n int) string {
	const digits = "0123456789ABCDEF"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[g.r.IntN(16)]
	}
	return string(b)
}

func pick[T any](r *rand.Rand, s []T) T { return s[r.IntN(len(s))] }
