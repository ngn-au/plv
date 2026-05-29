package main

import (
	"bufio"
	"context"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type Watcher struct {
	path           string
	store          *Store
	offset         atomic.Int64
	pending        map[string][]string
	pendingConnect string
	pendingTLS     string
	// Rolling content-filter verdicts keyed by session id. Filter lines arrive
	// before the Postfix "status=sent"/"removed" lines for the same message, so a
	// verdict is available by the time the message is finalized. verdictTS tracks
	// each entry's log timestamp so flushStale can prune them.
	verdicts  map[string]filterVerdict
	verdictTS map[string]time.Time
}

func NewWatcher(path string, store *Store, offset int64) *Watcher {
	w := &Watcher{
		path:      path,
		store:     store,
		pending:   make(map[string][]string),
		verdicts:  make(map[string]filterVerdict),
		verdictTS: make(map[string]time.Time),
	}
	w.offset.Store(offset)
	return w
}

// Offset returns the current tail position (bytes consumed from the active log).
func (w *Watcher) Offset() int64 { return w.offset.Load() }

func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	flushTicker := time.NewTicker(30 * time.Second)
	defer flushTicker.Stop()

	log.Printf("watcher: tailing %s from offset %d", w.path, w.offset.Load())

	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTicker.C:
			w.flushStale()
		case <-ticker.C:
			w.tick()
		}
	}
}

func (w *Watcher) tick() {
	info, err := os.Stat(w.path)
	if err != nil {
		return
	}

	off := w.offset.Load()
	if info.Size() < off {
		log.Printf("watcher: file rotated (size %d < offset %d), resetting", info.Size(), off)
		off = 0
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
		w.processLine(strings.TrimRight(line, "\n\r"))
	}
}

func (w *Watcher) processLine(line string) {
	if strings.Contains(line, "connect from ") && queueIDRe.FindString(line) == "" {
		w.pendingConnect = line
		return
	}
	if strings.Contains(line, "Trusted TLS connection established") && queueIDRe.FindString(line) == "" {
		w.pendingTLS = line
		return
	}

	// Accumulate content-filter verdicts for later correlation at finalize time.
	if fid, v, ok := parseFilterLine(line); ok {
		cur := w.verdicts[fid]
		mergeVerdict(&cur, v)
		w.verdicts[fid] = cur
		w.verdictTS[fid] = extractTimestamp(line)
		return
	}

	// SMTP-time rejections never get a queue id and never a "removed"; record now.
	if strings.Contains(line, "NOQUEUE: reject:") {
		if rec, ok := parseNoQueueReject(line); ok {
			w.store.AddRecords([]Record{rec})
		}
		return
	}

	qm := queueIDRe.FindStringSubmatch(line)
	if qm == nil {
		return
	}
	qid := qm[1]

	if _, exists := w.pending[qid]; !exists {
		if w.pendingConnect != "" {
			w.pending[qid] = append(w.pending[qid], w.pendingConnect)
			w.pendingConnect = ""
		}
	}
	if w.pendingTLS != "" {
		w.pending[qid] = append(w.pending[qid], w.pendingTLS)
		w.pendingTLS = ""
	}
	w.pending[qid] = append(w.pending[qid], line)

	if strings.Contains(line, ": removed") {
		w.finalize(qid)
	}
}

func (w *Watcher) finalize(qid string) {
	lines, exists := w.pending[qid]
	if !exists {
		return
	}
	delete(w.pending, qid)

	records := parseLines(lines)
	// Correlate the content-filter verdict accumulated from earlier lines.
	for i := range records {
		if fid := nestedFilterID(strings.Join(records[i].RawLines, " ")); fid != "" {
			if v, ok := w.verdicts[fid]; ok {
				enrichFromVerdict(&records[i], v)
			}
		}
	}
	if len(records) > 0 {
		w.store.AddRecords(records)
	}
}

// flushStale finalizes any pending queue IDs older than 5 minutes and prunes
// content-filter verdicts that are no longer likely to be referenced.
func (w *Watcher) flushStale() {
	now := time.Now()
	for qid, lines := range w.pending {
		if len(lines) == 0 {
			continue
		}
		ts := extractTimestamp(lines[0])
		if ts.IsZero() || now.Sub(ts) > 5*time.Minute {
			w.finalize(qid)
		}
	}
	for fid, ts := range w.verdictTS {
		if ts.IsZero() || now.Sub(ts) > 5*time.Minute {
			delete(w.verdicts, fid)
			delete(w.verdictTS, fid)
		}
	}
}
