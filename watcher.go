package main

import (
	"bufio"
	"context"
	"log"
	"os"
	"strings"
	"time"
)

type Watcher struct {
	path           string
	store          *Store
	offset         int64
	pending        map[string][]string
	pendingConnect string
	pendingTLS     string
}

func NewWatcher(path string, store *Store, offset int64) *Watcher {
	return &Watcher{
		path:    path,
		store:   store,
		offset:  offset,
		pending: make(map[string][]string),
	}
}

func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	flushTicker := time.NewTicker(30 * time.Second)
	defer flushTicker.Stop()

	log.Printf("watcher: tailing %s from offset %d", w.path, w.offset)

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

	if info.Size() < w.offset {
		log.Printf("watcher: file rotated (size %d < offset %d), resetting", info.Size(), w.offset)
		w.offset = 0
	}
	if info.Size() == w.offset {
		return
	}

	f, err := os.Open(w.path)
	if err != nil {
		return
	}
	defer f.Close()

	if _, err := f.Seek(w.offset, 0); err != nil {
		return
	}

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		w.offset += int64(len(line))
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
	if len(records) > 0 {
		w.store.AddRecords(records)
	}
}

// flushStale finalizes any pending queue IDs older than 5 minutes.
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
}
