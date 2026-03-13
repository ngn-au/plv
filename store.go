package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

type Record struct {
	Timestamp    time.Time
	QueueID      string
	From         string
	To           string
	Subject      string
	Size         int64
	MessageID    string
	Status       string
	StatusDetail string
	Relay        string
	Client       string
	TLS          string
	RawLines     []string
}

type Store struct {
	mu        sync.RWMutex
	records   []Record
	byQueueID map[string]int
	ready     bool
	status    string
	db        *DB
}

func NewStore(db *DB) *Store {
	return &Store{
		byQueueID: make(map[string]int),
		status:    "initializing",
		db:        db,
	}
}

func (s *Store) LoadFromDB() error {
	if s.db == nil {
		return nil
	}
	records, err := s.db.LoadAll()
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, r := range records {
		if _, exists := s.byQueueID[r.QueueID]; !exists {
			s.byQueueID[r.QueueID] = len(s.records)
			s.records = append(s.records, r)
		}
	}
	s.mu.Unlock()
	log.Printf("loaded %d records from database", len(records))
	return nil
}

func (s *Store) AddRecords(records []Record) {
	s.mu.Lock()
	var toUpsert []Record
	for _, r := range records {
		if idx, exists := s.byQueueID[r.QueueID]; exists {
			s.mergeRecord(&s.records[idx], &r)
			if s.db != nil {
				toUpsert = append(toUpsert, snapshotRecord(&s.records[idx]))
			}
		} else {
			s.byQueueID[r.QueueID] = len(s.records)
			s.records = append(s.records, r)
			if s.db != nil {
				toUpsert = append(toUpsert, snapshotRecord(&r))
			}
		}
	}
	s.mu.Unlock()

	if s.db != nil && len(toUpsert) > 0 {
		if err := s.db.UpsertRecords(toUpsert); err != nil {
			log.Printf("db persist error: %v", err)
		}
	}
}

func snapshotRecord(r *Record) Record {
	c := *r
	c.RawLines = make([]string, len(r.RawLines))
	copy(c.RawLines, r.RawLines)
	return c
}

func (s *Store) mergeRecord(dst, src *Record) {
	if dst.Timestamp.IsZero() && !src.Timestamp.IsZero() {
		dst.Timestamp = src.Timestamp
	}
	if dst.From == "" {
		dst.From = src.From
	}
	if dst.To == "" {
		dst.To = src.To
	}
	if dst.Subject == "" {
		dst.Subject = src.Subject
	}
	if dst.Size == 0 {
		dst.Size = src.Size
	}
	if dst.MessageID == "" {
		dst.MessageID = src.MessageID
	}
	if dst.Status == "" {
		dst.Status = src.Status
		dst.StatusDetail = src.StatusDetail
	}
	if dst.Relay == "" {
		dst.Relay = src.Relay
	}
	if dst.Client == "" {
		dst.Client = src.Client
	}
	if dst.TLS == "" {
		dst.TLS = src.TLS
	}
	if len(src.RawLines) > 0 {
		existing := make(map[string]struct{}, len(dst.RawLines))
		for _, l := range dst.RawLines {
			existing[l] = struct{}{}
		}
		for _, l := range src.RawLines {
			if _, dup := existing[l]; !dup {
				dst.RawLines = append(dst.RawLines, l)
			}
		}
	}
}

type RecordDetail struct {
	Timestamp    string       `json:"timestamp"`
	QueueID      string       `json:"queue_id"`
	From         string       `json:"from"`
	To           string       `json:"to"`
	Subject      string       `json:"subject"`
	Size         string       `json:"size"`
	SizeBytes    int64        `json:"size_bytes"`
	MessageID    string       `json:"message_id"`
	Status       string       `json:"status"`
	StatusDetail string       `json:"status_detail"`
	Relay        string       `json:"relay"`
	Client       string       `json:"client"`
	TLS          string       `json:"tls"`
	Lines        []LogLine    `json:"lines"`
}

type LogLine struct {
	Timestamp string `json:"timestamp"`
	Raw       string `json:"raw"`
}

func (s *Store) GetDetail(queueID string) *RecordDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()

	idx, ok := s.byQueueID[queueID]
	if !ok {
		return nil
	}
	r := &s.records[idx]

	ts := ""
	if !r.Timestamp.IsZero() {
		ts = r.Timestamp.UTC().Format(time.RFC3339)
	}

	lines := make([]LogLine, 0, len(r.RawLines))
	for _, raw := range r.RawLines {
		lineTs := extractTimestamp(raw)
		fmtTs := ""
		if !lineTs.IsZero() {
			fmtTs = lineTs.UTC().Format(time.RFC3339)
		}
		lines = append(lines, LogLine{Timestamp: fmtTs, Raw: raw})
	}

	return &RecordDetail{
		Timestamp:    ts,
		QueueID:      r.QueueID,
		From:         r.From,
		To:           r.To,
		Subject:      r.Subject,
		Size:         formatSize(r.Size),
		SizeBytes:    r.Size,
		MessageID:    r.MessageID,
		Status:       r.Status,
		StatusDetail: r.StatusDetail,
		Relay:        r.Relay,
		Client:       r.Client,
		TLS:          r.TLS,
		Lines:        lines,
	}
}

// PurgeOlderThan removes records with a timestamp older than the cutoff
// from both in-memory store and the database.
func (s *Store) PurgeOlderThan(d time.Duration) int {
	cutoff := time.Now().Add(-d)

	s.mu.Lock()
	var kept []Record
	newIndex := make(map[string]int)
	purged := 0
	for _, r := range s.records {
		if !r.Timestamp.IsZero() && r.Timestamp.Before(cutoff) {
			purged++
			continue
		}
		newIndex[r.QueueID] = len(kept)
		kept = append(kept, r)
	}
	s.records = kept
	s.byQueueID = newIndex
	s.mu.Unlock()

	if s.db != nil {
		if n, err := s.db.DeleteOlderThan(d); err != nil {
			log.Printf("db retention purge error: %v", err)
		} else if n > 0 {
			log.Printf("retention: purged %d records from database", n)
		}
	}

	return purged
}

func (s *Store) SetReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = true
	s.status = "ready"
}

func (s *Store) SetStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *Store) GetStatus() (bool, string, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready, s.status, len(s.records)
}

type SearchParams struct {
	Draw       int
	Start      int
	Length     int
	SearchTerm string
	OrderCol   int
	OrderDir   string

	FilterFrom    string
	FilterTo      string
	FilterClient  string
	FilterRelay   string
	FilterStatus  string
	FilterQueueID string
	FilterTLS     string // "yes", "no", or "" (any)
}

type SearchResult struct {
	Draw            int        `json:"draw"`
	RecordsTotal    int        `json:"recordsTotal"`
	RecordsFiltered int        `json:"recordsFiltered"`
	Data            [][]string `json:"data"`
}

func (s *Store) Search(p SearchParams) SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	search := strings.ToLower(p.SearchTerm)
	hasAdvanced := p.FilterFrom != "" || p.FilterTo != "" || p.FilterClient != "" ||
		p.FilterRelay != "" || p.FilterStatus != "" || p.FilterQueueID != "" || p.FilterTLS != ""

	var filtered []int
	for i, r := range s.records {
		if search != "" && !recordMatchesSearch(&r, search) {
			continue
		}
		if hasAdvanced && !recordMatchesAdvanced(&r, &p) {
			continue
		}
		filtered = append(filtered, i)
	}

	sort.Slice(filtered, func(a, b int) bool {
		ra, rb := &s.records[filtered[a]], &s.records[filtered[b]]
		cmp := compareByColumn(ra, rb, p.OrderCol)
		if p.OrderDir == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})

	total := len(filtered)
	start := p.Start
	end := start + p.Length
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	page := filtered[start:end]

	data := make([][]string, len(page))
	for i, idx := range page {
		data[i] = recordToRow(&s.records[idx])
	}

	return SearchResult{
		Draw:            p.Draw,
		RecordsTotal:    len(s.records),
		RecordsFiltered: total,
		Data:            data,
	}
}

func recordMatchesSearch(r *Record, search string) bool {
	fields := []string{
		r.QueueID, r.From, r.To, r.Subject,
		r.MessageID, r.Status, r.StatusDetail,
		r.Relay, r.Client, r.TLS,
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), search) {
			return true
		}
	}
	ts := r.Timestamp.UTC().Format("2006-01-02 15:04:05")
	if strings.Contains(ts, search) {
		return true
	}
	return false
}

func recordMatchesAdvanced(r *Record, p *SearchParams) bool {
	if p.FilterFrom != "" && !strings.Contains(strings.ToLower(r.From), strings.ToLower(p.FilterFrom)) {
		return false
	}
	if p.FilterTo != "" && !strings.Contains(strings.ToLower(r.To), strings.ToLower(p.FilterTo)) {
		return false
	}
	if p.FilterClient != "" && !strings.Contains(strings.ToLower(r.Client), strings.ToLower(p.FilterClient)) {
		return false
	}
	if p.FilterRelay != "" && !strings.Contains(strings.ToLower(r.Relay), strings.ToLower(p.FilterRelay)) {
		return false
	}
	if p.FilterStatus != "" && !strings.EqualFold(r.Status, p.FilterStatus) {
		return false
	}
	if p.FilterQueueID != "" && !strings.Contains(strings.ToLower(r.QueueID), strings.ToLower(p.FilterQueueID)) {
		return false
	}
	if p.FilterTLS == "yes" && r.TLS == "" {
		return false
	}
	if p.FilterTLS == "no" && r.TLS != "" {
		return false
	}
	return true
}

func compareByColumn(a, b *Record, col int) int {
	switch col {
	case 0:
		return a.Timestamp.Compare(b.Timestamp)
	case 1:
		return strings.Compare(a.QueueID, b.QueueID)
	case 2:
		return strings.Compare(strings.ToLower(a.From), strings.ToLower(b.From))
	case 3:
		return strings.Compare(strings.ToLower(a.To), strings.ToLower(b.To))
	case 4:
		if a.Size < b.Size {
			return -1
		} else if a.Size > b.Size {
			return 1
		}
		return 0
	case 5:
		return strings.Compare(a.Status, b.Status)
	case 6:
		return strings.Compare(a.Relay, b.Relay)
	case 7:
		return strings.Compare(a.Client, b.Client)
	default:
		return 0
	}
}

func recordToRow(r *Record) []string {
	ts := ""
	if !r.Timestamp.IsZero() {
		ts = r.Timestamp.UTC().Format(time.RFC3339)
	}
	status := r.Status
	if r.StatusDetail != "" {
		status += " " + r.StatusDetail
	}
	return []string{
		ts,
		r.QueueID,
		r.From,
		r.To,
		formatSize(r.Size),
		r.Status,
		r.StatusDetail,
		r.Relay,
		r.Client,
		r.MessageID,
		r.Subject,
		r.TLS,
	}
}

func formatSize(bytes int64) string {
	if bytes == 0 {
		return ""
	}
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}

type CountItem struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type HourlyItem struct {
	Hour  string `json:"hour"`
	Count int    `json:"count"`
}

type StatsResult struct {
	Total      int          `json:"total"`
	Sent       int          `json:"sent"`
	Rejected   int          `json:"rejected"`
	Bounced    int          `json:"bounced"`
	Deferred   int          `json:"deferred"`
	Other      int          `json:"other"`
	TopSenders []CountItem  `json:"top_senders"`
	TopRecipients []CountItem `json:"top_recipients"`
	Hourly     []HourlyItem `json:"hourly"`
	Ready      bool         `json:"ready"`
	Status     string       `json:"status"`
}

func (s *Store) Stats() StatsResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := StatsResult{
		Total:  len(s.records),
		Ready:  s.ready,
		Status: s.status,
	}

	senders := make(map[string]int)
	recipients := make(map[string]int)
	hourly := make(map[string]int)

	for i := range s.records {
		r := &s.records[i]
		st := strings.ToLower(r.Status)
		switch {
		case strings.HasPrefix(st, "sent"):
			result.Sent++
		case strings.HasPrefix(st, "reject"):
			result.Rejected++
		case strings.HasPrefix(st, "bounced"):
			result.Bounced++
		case strings.HasPrefix(st, "deferred"):
			result.Deferred++
		default:
			if st != "" {
				result.Other++
			}
		}
		if r.From != "" {
			senders[r.From]++
		}
		if r.To != "" {
			recipients[r.To]++
		}
		if !r.Timestamp.IsZero() {
			hourly[r.Timestamp.Format("2006-01-02 15")]++
		}
	}

	result.TopSenders = topN(senders, 10)
	result.TopRecipients = topN(recipients, 10)
	result.Hourly = sortedHourly(hourly)
	return result
}

func topN(counts map[string]int, n int) []CountItem {
	items := make([]CountItem, 0, len(counts))
	for name, count := range counts {
		items = append(items, CountItem{Name: name, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Count > items[j].Count
	})
	if len(items) > n {
		items = items[:n]
	}
	return items
}

func sortedHourly(counts map[string]int) []HourlyItem {
	items := make([]HourlyItem, 0, len(counts))
	for hour, count := range counts {
		items = append(items, HourlyItem{Hour: hour, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Hour < items[j].Hour
	})
	return items
}
