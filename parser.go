package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	queueIDRe = regexp.MustCompile(`postfix/[^\]]+\]:\s*([A-Fa-f0-9]{10,12}):`)

	reTo        = regexp.MustCompile(`to=<([^>]*)>,.*orig_to=<([^>]*)>|to=<([^>]*)>`)
	reFrom      = regexp.MustCompile(`from=<([a-zA-Z0-9\-+_.=]+@[a-zA-Z0-9\-+_.]+)>`)
	reSubject   = regexp.MustCompile(`header\sSubject:\s(.*?)\sfrom\s\S+\[`)
	reSize      = regexp.MustCompile(`size=([0-9]+),`)
	reMessageID = regexp.MustCompile(`message-id=<([^>]*)>`)
	reStatus    = regexp.MustCompile(`status=([a-zA-Z0-9\-_.]+)\s(\([^)]*\))|(reject):\s[^:]*:\s([0-9][^;]*);\s`)
	reRelay     = regexp.MustCompile(`(?:relay|connect to).([a-zA-Z0-9\-._]+)\[([^\]]*)\]:([0-9]+)`)
	reClient    = regexp.MustCompile(`client.([a-zA-Z0-9\-._]+)?\[([^\]]*)\]`)
	// After a content filter re-injects a message, postfix logs the real external
	// origin as orig_client= alongside client=localhost.
	reOrigClient = regexp.MustCompile(`orig_client=([a-zA-Z0-9\-._]+)?\[([^\]]*)\]`)
	reConnect    = regexp.MustCompile(`connect from ([a-zA-Z0-9\-._]+)\[([^\]]+)\]`)
	reTLS       = regexp.MustCompile(`Trusted TLS connection established to ([^[]+)\[([^\]]+)\]:([0-9]+)`)
	reTimestamp = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\S+)`)

	// Content-filter (Proxmox Mail Gateway pmg-smtp-filter) correlation.
	// reFilterTag identifies a filter line and captures its session id.
	reFilterTag = regexp.MustCompile(`pmg-smtp-filter\[\d+\]:\s*([A-Fa-f0-9]{14,}):`)
	// reFilterHandoff pulls the filter session id out of the Postfix LMTP/SMTP
	// hand-off detail, e.g. status=sent (250 2.5.0 OK (C0FFEE0000000001)).
	reFilterHandoff = regexp.MustCompile(`status=sent \(\d{3} [0-9.]+ \w+ \(([A-Fa-f0-9]{6,})\)\)`)
	reSAScore       = regexp.MustCompile(`SA score=([0-9.]+)/([0-9.]+)`)
	// The rule text can itself contain parentheses, e.g. "Quarantine/Mark Spam
	// (Level 3)", so capture greedily up to the final ")" on the line.
	reQuarantine = regexp.MustCompile(`to ([a-z]+) quarantine - ([A-Fa-f0-9]+) \(rule: (.+)\)`)
	reAccept     = regexp.MustCompile(`accept mail to <[^>]*> \(([A-Fa-f0-9]+)\) \(rule: (.+)\)`)
	reBlock         = regexp.MustCompile(`block mail to `)
	// reNoQueue parses SMTP-time rejections that never get a queue id
	// (postscreen/smtpd "NOQUEUE: reject"). host is optional (postscreen logs
	// "from [ip]:port" with no hostname).
	reNoQueue = regexp.MustCompile(`NOQUEUE: reject: (\w+) from (\S*?)\[([^\]]*)\](?::\d+)?: (\d{3}[^;]*);`)
)

// filterVerdict is the outcome a content filter reached for one scanning session,
// accumulated across its several log lines (SA score, then accept/quarantine/block).
type filterVerdict struct {
	filterID     string
	action       string // "spam quarantine"|"virus quarantine"|"accept"|"block"
	disposition  string // mapped onto Record.Disposition
	rule         string
	score        string // "8/5" (score/threshold)
	quarantineID string // target id for a quarantine action
	onwardQID    string // postfix queue id of the onward leg after "accept"
	rawLines     []string
}

// parseFilterLine recognizes a pmg-smtp-filter line carrying a session id. Every
// such line is captured (for the detail view); lines that also carry a verdict
// (SA score, accept/quarantine/block) fill in the verdict fields. Lines for the
// same session id are folded together with mergeVerdict.
func parseFilterLine(line string) (string, filterVerdict, bool) {
	m := reFilterTag.FindStringSubmatch(line)
	if m == nil {
		return "", filterVerdict{}, false
	}
	fid := m[1]
	v := filterVerdict{filterID: fid, rawLines: []string{line}}

	if sm := reSAScore.FindStringSubmatch(line); sm != nil {
		v.score = sm[1] + "/" + sm[2]
	}

	if qm := reQuarantine.FindStringSubmatch(line); qm != nil {
		v.action = qm[1] + " quarantine"
		v.quarantineID = qm[2]
		v.rule = qm[3]
		if qm[1] == "virus" {
			v.disposition = "virus"
		} else {
			v.disposition = "spam"
		}
	} else if am := reAccept.FindStringSubmatch(line); am != nil {
		v.action = "accept"
		v.onwardQID = am[1]
		v.rule = am[2]
		v.disposition = "sent"
	} else if reBlock.MatchString(line) {
		v.action = "block"
		v.disposition = "blocked"
	}

	return fid, v, true
}

// mergeVerdict folds a per-line verdict into the accumulated one for a session id.
func mergeVerdict(dst *filterVerdict, src filterVerdict) {
	if dst.filterID == "" {
		dst.filterID = src.filterID
	}
	if src.score != "" {
		dst.score = src.score
	}
	if src.action != "" {
		dst.action = src.action
		dst.disposition = src.disposition
		dst.rule = src.rule
		dst.quarantineID = src.quarantineID
		dst.onwardQID = src.onwardQID
	}
	dst.rawLines = append(dst.rawLines, src.rawLines...)
}

// nestedFilterID extracts the content-filter session id from a Postfix hand-off line.
func nestedFilterID(blob string) string {
	m := reFilterHandoff.FindStringSubmatch(blob)
	if m == nil {
		return ""
	}
	return m[1]
}

// enrichFromVerdict applies a content-filter verdict to a Postfix record.
func enrichFromVerdict(rec *Record, v filterVerdict) {
	rec.Filter = "pmg-smtp-filter"
	rec.FilterID = v.filterID
	rec.FilterAction = v.action
	rec.SpamScore = v.score
	rec.FilterRule = v.rule
	// When accepted, the scanner re-injects under a new postfix queue id; record
	// it so the inbound and outbound legs can be merged into one item.
	if v.action == "accept" && v.onwardQID != "" {
		rec.DeliveryQueueID = v.onwardQID
	}
	// Include the scanner's own log lines in the record's timeline.
	if len(v.rawLines) > 0 {
		rec.RawLines = sortRawByTime(append(rec.RawLines, v.rawLines...))
	}
	if v.disposition != "" {
		rec.Disposition = v.disposition
	} else {
		rec.Disposition = deriveDisposition(rec)
	}
}

// sortRawByTime returns the raw log lines ordered by their leading timestamp,
// de-duplicated, so a merged record reads as one chronological timeline.
func sortRawByTime(lines []string) []string {
	seen := make(map[string]struct{}, len(lines))
	uniq := make([]string, 0, len(lines))
	for _, l := range lines {
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		uniq = append(uniq, l)
	}
	sort.SliceStable(uniq, func(i, j int) bool {
		return extractTimestamp(uniq[i]).Before(extractTimestamp(uniq[j]))
	})
	return uniq
}

// deriveDisposition maps a record with no explicit filter verdict onto an effective
// outcome, classifying spam/virus rejections (e.g. milter "Blocked by SpamAssassin").
func deriveDisposition(rec *Record) string {
	st := strings.ToLower(rec.Status)
	detail := strings.ToLower(rec.StatusDetail)
	switch {
	case strings.HasPrefix(st, "sent"):
		return "sent"
	case strings.HasPrefix(st, "reject"):
		if strings.Contains(detail, "spamassassin") || strings.Contains(detail, "spam") {
			return "spam"
		}
		if strings.Contains(detail, "virus") {
			return "virus"
		}
		return "rejected"
	case strings.HasPrefix(st, "bounced"):
		return "bounced"
	case strings.HasPrefix(st, "deferred"):
		return "deferred"
	case st == "":
		return "received"
	default:
		return st
	}
}

// parseNoQueueReject turns a "NOQUEUE: reject" line into a standalone record with a
// deterministic synthetic queue id so re-parses dedupe instead of duplicating.
func parseNoQueueReject(line string) (Record, bool) {
	m := reNoQueue.FindStringSubmatch(line)
	if m == nil {
		return Record{}, false
	}
	stage, host, ip, smtpResp := m[1], m[2], m[3], strings.TrimSpace(m[4])

	// A "NOQUEUE: reject" is a rejection regardless of the SMTP code — postfix
	// issues 4xx for some policy rejections (e.g. "Sender address rejected:
	// Domain not found"), but the message was still refused, not merely deferred.
	disposition := "rejected"

	client := ""
	if host != "" || ip != "" {
		client = fmt.Sprintf("%s[%s]", host, ip)
	}

	rec := Record{
		Timestamp:    extractTimestamp(line),
		QueueID:      syntheticID("N", line),
		From:         extractFrom(line),
		To:           extractTo(line),
		Status:       "reject",
		StatusDetail: smtpResp,
		Client:       client,
		Disposition:  disposition,
		FilterAction: "noqueue-reject",
		FilterRule:   stage,
		RawLines:     []string{line},
	}
	return rec, true
}

// syntheticID builds a stable id from a raw log line for events Postfix never
// assigns a queue id to (NOQUEUE rejects).
func syntheticID(prefix, line string) string {
	h := fnv.New64a()
	h.Write([]byte(line))
	return fmt.Sprintf("%s%X", prefix, h.Sum64())
}

func extractTimestamp(text string) time.Time {
	m := reTimestamp.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func extractTo(text string) string {
	m := reTo.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	if m[3] != "" {
		return strings.TrimSpace(m[3])
	}
	if m[1] != "" {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(m[2])
}

func extractFrom(text string) string {
	m := reFrom.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func extractSubject(text string) string {
	m := reSubject.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	raw := strings.TrimSpace(m[1])
	dec := new(mime.WordDecoder)
	if decoded, err := dec.DecodeHeader(raw); err == nil {
		return decoded
	}
	return raw
}

func extractSize(text string) int64 {
	m := reSize.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(m[1]), 10, 64)
	return n
}

func extractMessageID(text string) string {
	m := reMessageID.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func extractStatus(text string) (string, string) {
	m := reStatus.FindStringSubmatch(text)
	if m == nil {
		return "", ""
	}
	code := strings.TrimSpace(m[1])
	detail := strings.TrimSpace(m[2])
	if code == "" {
		code = strings.TrimSpace(m[3])
		detail = strings.TrimSpace(m[4])
	}
	return code, detail
}

func extractRelay(text string) (string, string, string) {
	m := reRelay.FindStringSubmatch(text)
	if m == nil {
		return "", "", ""
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
}

func extractClient(text string) (string, string) {
	m := reClient.FindStringSubmatch(text)
	if m == nil {
		return "", ""
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
}

func extractConnect(text string) (string, string) {
	m := reConnect.FindStringSubmatch(text)
	if m == nil {
		return "", ""
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
}

func extractOrigClient(text string) (string, string) {
	m := reOrigClient.FindStringSubmatch(text)
	if m == nil {
		return "", ""
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
}

func extractTLS(text string) (string, string, string) {
	m := reTLS.FindStringSubmatch(text)
	if m == nil {
		return "", "", ""
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
}

// parseLines groups raw log lines by queue ID and returns one Record per message.
func parseLines(lines []string) []Record {
	type group struct {
		lines []string
		order int
	}
	groups := make(map[string]*group)
	counter := 0
	var pendingConnect, pendingTLS string

	// Pass 1 accumulators: content-filter verdicts keyed by session id, and
	// standalone records synthesized from NOQUEUE rejections.
	verdicts := make(map[string]filterVerdict)
	var noqueue []Record

	for _, line := range lines {
		if strings.Contains(line, "connect from ") && queueIDRe.FindString(line) == "" {
			pendingConnect = line
			continue
		}
		if strings.Contains(line, "Trusted TLS connection established") && queueIDRe.FindString(line) == "" {
			pendingTLS = line
			continue
		}

		// Content-filter (pmg-smtp-filter) lines carry their own session id, not a
		// Postfix queue id; collect their verdicts for correlation in pass 2.
		if fid, v, ok := parseFilterLine(line); ok {
			cur := verdicts[fid]
			mergeVerdict(&cur, v)
			verdicts[fid] = cur
			continue
		}

		// SMTP-time rejections never get a queue id; emit a standalone record.
		if strings.Contains(line, "NOQUEUE: reject:") {
			if rec, ok := parseNoQueueReject(line); ok {
				noqueue = append(noqueue, rec)
			}
			continue
		}

		qm := queueIDRe.FindStringSubmatch(line)
		if qm == nil {
			continue
		}
		qid := qm[1]

		g, exists := groups[qid]
		if !exists {
			g = &group{order: counter}
			counter++
			groups[qid] = g
			if pendingConnect != "" {
				g.lines = append(g.lines, pendingConnect)
				pendingConnect = ""
			}
		}
		if pendingTLS != "" {
			g.lines = append(g.lines, pendingTLS)
			pendingTLS = ""
		}
		g.lines = append(g.lines, line)
	}

	sorted := make([]string, 0, len(groups))
	for qid := range groups {
		sorted = append(sorted, qid)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return groups[sorted[i]].order < groups[sorted[j]].order
	})

	records := make([]Record, 0, len(sorted))
	for _, qid := range sorted {
		g := groups[qid]
		blob := strings.Join(g.lines, " ")

		ts := time.Time{}
		if len(g.lines) > 0 {
			ts = extractTimestamp(g.lines[0])
		}

		toAddr := extractTo(blob)
		fromAddr := extractFrom(blob)
		subject := extractSubject(blob)
		size := extractSize(blob)
		messageID := extractMessageID(blob)
		statusCode, statusDetail := extractStatus(blob)
		relayHost, relayIP, relayPort := extractRelay(blob)
		clientHost, clientIP := extractClient(blob)
		connectHost, connectIP := extractConnect(blob)
		tlsHost, tlsIP, tlsPort := extractTLS(blob)

		if clientHost == "" && clientIP == "" {
			clientHost, clientIP = connectHost, connectIP
		}
		// Prefer the real external origin when the message was re-injected locally.
		if oh, oi := extractOrigClient(blob); oh != "" || oi != "" {
			clientHost, clientIP = oh, oi
		}

		relay := ""
		if relayHost != "" || relayIP != "" {
			relay = fmt.Sprintf("%s[%s]:%s", relayHost, relayIP, relayPort)
		}
		client := ""
		if clientHost != "" || clientIP != "" {
			client = fmt.Sprintf("%s[%s]", clientHost, clientIP)
		}
		tlsInfo := ""
		if tlsHost != "" || tlsIP != "" {
			tlsInfo = fmt.Sprintf("%s[%s]:%s", tlsHost, tlsIP, tlsPort)
		}

		rawCopy := make([]string, len(g.lines))
		copy(rawCopy, g.lines)

		rec := Record{
			Timestamp:    ts,
			QueueID:      qid,
			From:         fromAddr,
			To:           toAddr,
			Subject:      subject,
			Size:         size,
			MessageID:    messageID,
			Status:       statusCode,
			StatusDetail: statusDetail,
			Relay:        relay,
			Client:       client,
			TLS:          tlsInfo,
			RawLines:     rawCopy,
		}

		// Correlate a local content-filter verdict if this message was handed off
		// to one; otherwise classify from the Postfix status alone.
		if fid := nestedFilterID(blob); fid != "" {
			if v, ok := verdicts[fid]; ok {
				enrichFromVerdict(&rec, v)
			}
		}
		if rec.Disposition == "" {
			rec.Disposition = deriveDisposition(&rec)
		}

		records = append(records, rec)
	}

	records = append(records, noqueue...)
	return records
}

// parseFile reads a single log file (plain or gzip) and returns records.
func parseFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var reader io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip open %s: %w", path, err)
		}
		defer gz.Close()
		reader = gz
	}

	var lines []string
	scanner := bufio.NewScanner(reader)
	buf := make([]byte, 0, 256*1024)
	scanner.Buffer(buf, 10*1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return parseLines(lines), nil
}

// discoverLogFiles finds all mail.log* files in the directory,
// sorted oldest first (highest rotation number first, mail.log last).
func discoverLogFiles(logDir string) []string {
	pattern := filepath.Join(logDir, "mail.log*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		log.Printf("glob error: %v", err)
		return nil
	}

	base := filepath.Join(logDir, "mail.log")

	sort.Slice(matches, func(i, j int) bool {
		ni := logFileNumber(matches[i], base)
		nj := logFileNumber(matches[j], base)
		if ni == 0 {
			return false
		}
		if nj == 0 {
			return true
		}
		return ni > nj
	})

	return matches
}

// logFileNumber extracts the rotation number from a mail.log filename.
// mail.log -> 0, mail.log.1 -> 1, mail.log.2.gz -> 2
func logFileNumber(path, base string) int {
	suffix := strings.TrimPrefix(path, base)
	if suffix == "" {
		return 0
	}
	suffix = strings.TrimPrefix(suffix, ".")
	suffix = strings.TrimSuffix(suffix, ".gz")
	n, err := strconv.Atoi(suffix)
	if err != nil {
		return -1
	}
	return n
}

// parseAllLogs discovers and parses all mail.log* files in order.
// Returns the byte offset of the current mail.log after parsing.
func parseAllLogs(logDir string, store *Store) int64 {
	files := discoverLogFiles(logDir)
	if len(files) == 0 {
		log.Printf("no mail.log files found in %s", logDir)
		return 0
	}

	log.Printf("found %d log file(s) to parse", len(files))
	for i, path := range files {
		name := filepath.Base(path)
		store.SetStatus(fmt.Sprintf("parsing %s (%d/%d)", name, i+1, len(files)))
		log.Printf("parsing %s (%d/%d)...", name, i+1, len(files))

		records, err := parseFile(path)
		if err != nil {
			log.Printf("error parsing %s: %v", path, err)
			continue
		}
		store.AddRecords(records)
		log.Printf("  -> %d records (total: %d)", len(records), func() int {
			_, _, n := store.GetStatus()
			return n
		}())
	}

	currentLog := filepath.Join(logDir, "mail.log")
	info, err := os.Stat(currentLog)
	if err != nil {
		return 0
	}
	return info.Size()
}
