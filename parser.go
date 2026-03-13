package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
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
	reConnect   = regexp.MustCompile(`connect from ([a-zA-Z0-9\-._]+)\[([^\]]+)\]`)
	reTLS       = regexp.MustCompile(`Trusted TLS connection established to ([^[]+)\[([^\]]+)\]:([0-9]+)`)
	reTimestamp = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\S+)`)
)

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

	for _, line := range lines {
		if strings.Contains(line, "connect from ") && queueIDRe.FindString(line) == "" {
			pendingConnect = line
			continue
		}
		if strings.Contains(line, "Trusted TLS connection established") && queueIDRe.FindString(line) == "" {
			pendingTLS = line
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

		records = append(records, Record{
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
		})
	}

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
