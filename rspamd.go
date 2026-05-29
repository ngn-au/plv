package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// rspamd integration: on an rspamd host, the scanner logs its verdict to its own
// /var/log/rspamd/rspamd.log (not the postfix mail.log). Each task summary line
// carries the postfix queue id (qid:), so we correlate the verdict back to the
// exact mail record — only enriching mail that already exists in the mail log,
// never creating standalone rows from rspamd lines.
var (
	reRspamdQID    = regexp.MustCompile(`qid: <([A-Za-z0-9]+)>`)
	reRspamdID     = regexp.MustCompile(`\bid: <([^>]*)>`)
	reRspamdAction = regexp.MustCompile(`\(default: \S+ \(([a-z ]+)\): \[(-?[0-9.]+)/`)
	reRspamdTime   = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
)

type rspamdVerdict struct {
	action    string // "no action"|"add header"|"rewrite subject"|"greylist"|"soft reject"|"reject"
	score     string
	messageID string
	rawLine   string
}

// parseRspamdLine extracts the verdict from an rspamd task summary line and the
// postfix queue id it applies to.
func parseRspamdLine(line string) (qid string, v rspamdVerdict, ok bool) {
	if !strings.Contains(line, "rspamd_task_write_log") {
		return "", rspamdVerdict{}, false
	}
	qm := reRspamdQID.FindStringSubmatch(line)
	if qm == nil || qm[1] == "" {
		return "", rspamdVerdict{}, false // pre-queue (no qid) — nothing to join to
	}
	am := reRspamdAction.FindStringSubmatch(line)
	if am == nil {
		return "", rspamdVerdict{}, false
	}
	v = rspamdVerdict{action: am[1], score: am[2], rawLine: line}
	if im := reRspamdID.FindStringSubmatch(line); im != nil {
		v.messageID = im[1]
	}
	return qm[1], v, true
}

// rspamdDisposition maps an rspamd action onto an effective disposition. Only "reject"
// actually stops the mail; "add header" and "rewrite subject" merely tag a message that
// is then DELIVERED, so they leave the postfix-derived disposition (e.g. delivered)
// unchanged — the spam score still rides along on the record. "no action" is a no-op too.
func rspamdDisposition(action string) string {
	switch action {
	case "reject":
		return "spam"
	case "soft reject", "greylist":
		return "deferred"
	default:
		return ""
	}
}

// enrichRecordWithRspamd attaches an rspamd verdict to a correlated mail record.
func enrichRecordWithRspamd(rec *Record, v rspamdVerdict) {
	rec.Filter = "rspamd"
	rec.FilterAction = v.action
	if v.score != "" {
		rec.SpamScore = v.score
	}
	if rec.MessageID == "" && v.messageID != "" {
		rec.MessageID = v.messageID
	}
	if disp := rspamdDisposition(v.action); disp != "" {
		rec.Disposition = disp
	}
	if v.rawLine != "" {
		for _, l := range rec.RawLines {
			if l == v.rawLine {
				return
			}
		}
		rec.RawLines = append(rec.RawLines, v.rawLine) // scanner line carries the symbol breakdown
	}
}

func extractRspamdTime(line string) time.Time {
	return extractRspamdTimeIn(line, time.Local)
}

// extractRspamdTimeIn parses rspamd's zone-less wall-clock in the given location. rspamd
// logs no offset (unlike Postfix's RFC3339), so to line a verdict up with its mail
// record's timeline we interpret it in that record's zone — passing time.Local only as a
// fallback when the record's offset isn't known.
func extractRspamdTimeIn(line string, loc *time.Location) time.Time {
	m := reRspamdTime.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}
	}
	if loc == nil {
		loc = time.Local
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

// discoverRspamdLogs finds rspamd.log* files under the mounted log dir, in the
// standard /var/log/rspamd/ subdir or directly in the log dir. Returns oldest first.
func discoverRspamdLogs(logDir string) []string {
	var matches []string
	for _, pat := range []string{
		filepath.Join(logDir, "rspamd", "rspamd.log*"),
		filepath.Join(logDir, "rspamd.log*"),
	} {
		if m, err := filepath.Glob(pat); err == nil {
			matches = append(matches, m...)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		ni := rspamdFileNumber(matches[i])
		nj := rspamdFileNumber(matches[j])
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

func rspamdFileNumber(path string) int {
	return logFileNumber(path, filepath.Join(filepath.Dir(path), "rspamd.log"))
}

// activeRspamdLog returns the path to the live rspamd.log, if present.
func activeRspamdLog(logDir string) string {
	for _, p := range []string{
		filepath.Join(logDir, "rspamd", "rspamd.log"),
		filepath.Join(logDir, "rspamd.log"),
	} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// parseAllRspamd parses every rspamd.log* file and correlates verdicts to records.
func parseAllRspamd(logDir string, store *Store) {
	files := discoverRspamdLogs(logDir)
	if len(files) == 0 {
		return
	}
	log.Printf("found %d rspamd log file(s) to correlate", len(files))
	applied := 0
	for _, path := range files {
		applied += parseRspamdFile(path, store)
	}
	log.Printf("rspamd: correlated %d verdicts to mail records", applied)
}

func parseRspamdFile(path string, store *Store) int {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("error opening %s: %v", path, err)
		return 0
	}
	defer f.Close()

	var reader io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			log.Printf("gzip open %s: %v", path, err)
			return 0
		}
		defer gz.Close()
		reader = gz
	}

	applied := 0
	scanner := bufio.NewScanner(reader)
	buf := make([]byte, 0, 256*1024)
	scanner.Buffer(buf, 10*1024*1024)
	for scanner.Scan() {
		if qid, v, ok := parseRspamdLine(scanner.Text()); ok {
			if store.ApplyRspamdVerdict(qid, v) {
				applied++
			}
		}
	}
	return applied
}

// RspamdWatcher live-tails the active rspamd.log and correlates new verdicts.
type RspamdWatcher struct {
	path   string
	store  *Store
	offset atomic.Int64
}

func NewRspamdWatcher(path string, store *Store, offset int64) *RspamdWatcher {
	w := &RspamdWatcher{path: path, store: store}
	w.offset.Store(offset)
	return w
}

// Offset returns the current tail position of the active rspamd.log.
func (w *RspamdWatcher) Offset() int64 { return w.offset.Load() }

func (w *RspamdWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	pruneTicker := time.NewTicker(5 * time.Minute)
	defer pruneTicker.Stop()

	log.Printf("rspamd watcher: tailing %s from offset %d", w.path, w.offset.Load())
	for {
		select {
		case <-ctx.Done():
			return
		case <-pruneTicker.C:
			w.store.PruneRspamdPending(15 * time.Minute)
		case <-ticker.C:
			w.tick()
		}
	}
}

func (w *RspamdWatcher) tick() {
	info, err := os.Stat(w.path)
	if err != nil {
		return
	}
	off := w.offset.Load()
	if info.Size() < off {
		off = 0 // rotated
		w.offset.Store(0)
	}
	if info.Size() == off {
		return
	}
	f, err := os.Open(w.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(off, 0); err != nil {
		return
	}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		off += int64(len(line))
		w.offset.Store(off)
		if qid, v, ok := parseRspamdLine(strings.TrimRight(line, "\n\r")); ok {
			w.store.ApplyRspamdVerdict(qid, v)
		}
	}
}
