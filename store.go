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

	// Derived content-filter / scanner verdict (PMG pmg-smtp-filter, SpamAssassin
	// milter, NOQUEUE rejects). Disposition is the effective outcome shown as the
	// badge; the raw Status/StatusDetail above are preserved untouched.
	Disposition  string // delivered|sent|spam|blocked|virus|rejected|bounced|deferred|received|other
	Filter       string // scanner name, e.g. "pmg-smtp-filter"
	FilterAction string // raw verb: "spam quarantine"|"accept"|"milter-reject"|"noqueue-reject"|"block"
	SpamScore    string // e.g. "8/5" (score/threshold)
	FilterRule   string // e.g. "Quarantine/Mark Spam (Level 3)"
	FilterID     string // pmg-smtp-filter session id, for cross-ref/search

	// After a content filter accepts a message it re-injects it under a new queue
	// id for final delivery. DeliveryQueueID links the inbound (scanner) leg to
	// that outbound leg; the two are merged into one item. Subsumed marks the
	// outbound leg when it was merged into its inbound primary.
	DeliveryQueueID string
	Subsumed        bool
}

type Store struct {
	mu        sync.RWMutex
	records   []Record
	byQueueID map[string]int
	// byDelivery maps an onward (post-filter) queue id to the index of the inbound
	// primary record that expects it, so the two legs can be merged.
	byDelivery map[string]int
	// rspamdPending holds rspamd verdicts whose mail record hasn't been seen yet
	// (the verdict can arrive just before the record is finalized in live mode).
	rspamdPending map[string]rspamdVerdict
	ready         bool
	status        string
	db            *DB
}

func NewStore(db *DB) *Store {
	return &Store{
		byQueueID:     make(map[string]int),
		byDelivery:    make(map[string]int),
		rspamdPending: make(map[string]rspamdVerdict),
		status:        "initializing",
		db:            db,
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
		if r.Subsumed {
			continue // merged into another item; not loaded as its own row
		}
		if _, exists := s.byQueueID[r.QueueID]; !exists {
			idx := len(s.records)
			s.byQueueID[r.QueueID] = idx
			s.records = append(s.records, r)
			// Restore the delivery link so the outbound queue id resolves to this
			// merged primary for search/detail.
			if r.DeliveryQueueID != "" {
				s.byDelivery[r.DeliveryQueueID] = idx
				if _, ok := s.byQueueID[r.DeliveryQueueID]; !ok {
					s.byQueueID[r.DeliveryQueueID] = idx
				}
			}
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
		// (1) A re-injected outbound leg whose inbound primary we already have:
		// fold the final delivery into the primary instead of adding a new row.
		if pIdx, ok := s.byDelivery[r.QueueID]; ok && pIdx < len(s.records) {
			mergeDeliveryLeg(&s.records[pIdx], &r)
			s.byQueueID[r.QueueID] = pIdx // resolve the outbound id to the primary
			if s.db != nil {
				toUpsert = append(toUpsert, snapshotRecord(&s.records[pIdx]))
			}
			continue
		}

		// (2) Same queue id seen again: merge incrementally (as before).
		if idx, exists := s.byQueueID[r.QueueID]; exists && !s.records[idx].Subsumed {
			s.mergeRecord(&s.records[idx], &r)
			if r.DeliveryQueueID != "" {
				s.registerDelivery(idx, &toUpsert)
			}
			s.applyPendingRspamd(idx)
			if s.db != nil {
				toUpsert = append(toUpsert, snapshotRecord(&s.records[idx]))
			}
			continue
		}

		// (3) New record.
		idx := len(s.records)
		s.byQueueID[r.QueueID] = idx
		s.records = append(s.records, r)
		if r.DeliveryQueueID != "" {
			s.registerDelivery(idx, &toUpsert)
		}
		s.applyPendingRspamd(idx)
		if s.db != nil {
			toUpsert = append(toUpsert, snapshotRecord(&s.records[idx]))
		}
	}
	s.mu.Unlock()

	if s.db != nil && len(toUpsert) > 0 {
		if err := s.db.UpsertRecords(toUpsert); err != nil {
			log.Printf("db persist error: %v", err)
		}
	}
}

// ApplyRspamdVerdict correlates an rspamd verdict to its mail record by queue id.
// If the record exists it is enriched immediately; otherwise the verdict is held
// (briefly) until the record is added. Returns true when it matched a record.
func (s *Store) ApplyRspamdVerdict(qid string, v rspamdVerdict) bool {
	s.mu.Lock()
	idx, ok := s.byQueueID[qid]
	if ok && idx < len(s.records) && !s.records[idx].Subsumed {
		enrichRecordWithRspamd(&s.records[idx], v)
		var snap Record
		if s.db != nil {
			snap = snapshotRecord(&s.records[idx])
		}
		s.mu.Unlock()
		if s.db != nil {
			if err := s.db.UpsertRecords([]Record{snap}); err != nil {
				log.Printf("db persist error: %v", err)
			}
		}
		return true
	}
	s.rspamdPending[qid] = v
	s.mu.Unlock()
	return false
}

// PruneRspamdPending drops pending rspamd verdicts older than maxAge (they had no
// matching mail record — e.g. pre-queue activity).
func (s *Store) PruneRspamdPending(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	s.mu.Lock()
	defer s.mu.Unlock()
	for qid, v := range s.rspamdPending {
		ts := extractRspamdTime(v.rawLine)
		if ts.IsZero() || ts.Before(cutoff) {
			delete(s.rspamdPending, qid)
		}
	}
}

// applyPendingRspamd enriches the record at idx with a pending rspamd verdict for
// its queue id, if one arrived before the record. Caller holds s.mu.
func (s *Store) applyPendingRspamd(idx int) {
	qid := s.records[idx].QueueID
	if v, ok := s.rspamdPending[qid]; ok {
		enrichRecordWithRspamd(&s.records[idx], v)
		delete(s.rspamdPending, qid)
	}
}

// registerDelivery records that the primary at idx expects an outbound leg. If that
// leg already arrived as a standalone row (rare; legs normally follow the primary),
// merge it in now and mark it subsumed. Caller holds s.mu.
func (s *Store) registerDelivery(idx int, toUpsert *[]Record) {
	deliveryID := s.records[idx].DeliveryQueueID
	s.byDelivery[deliveryID] = idx
	if oIdx, ok := s.byQueueID[deliveryID]; ok && oIdx != idx && !s.records[oIdx].Subsumed {
		mergeDeliveryLeg(&s.records[idx], &s.records[oIdx])
		s.records[oIdx].Subsumed = true
		s.byQueueID[deliveryID] = idx
		if s.db != nil {
			*toUpsert = append(*toUpsert, snapshotRecord(&s.records[oIdx]))
		}
	}
}

// mergeDeliveryLeg folds an outbound (post-filter) delivery leg into its inbound
// primary: the primary keeps its rich metadata (subject, original client, scanner
// verdict) and gains the real destination relay and final delivery status.
func mergeDeliveryLeg(primary, leg *Record) {
	primary.DeliveryQueueID = leg.QueueID
	if leg.Relay != "" {
		primary.Relay = leg.Relay // real destination overwrites 127.0.0.1 scanner
	}
	if leg.Status != "" {
		primary.Status = leg.Status
		primary.StatusDetail = leg.StatusDetail
		primary.Disposition = deriveDisposition(leg) // final delivery outcome
	}
	if primary.MessageID == "" {
		primary.MessageID = leg.MessageID
	}
	if primary.Subject == "" {
		primary.Subject = leg.Subject
	}
	if primary.From == "" {
		primary.From = leg.From
	}
	if primary.To == "" {
		primary.To = leg.To
	}
	if len(leg.RawLines) > 0 {
		primary.RawLines = sortRawByTime(append(primary.RawLines, leg.RawLines...))
	}
}

// dispositionOrStatus returns the derived Disposition, falling back to the first
// word of the raw Postfix Status for records that predate filter correlation
// (e.g. legacy rows loaded from the database).
func (r *Record) dispositionOrStatus() string {
	if r.Disposition != "" {
		return r.Disposition
	}
	if fields := strings.Fields(r.Status); len(fields) > 0 {
		return strings.ToLower(fields[0])
	}
	return ""
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
	if dst.Disposition == "" {
		dst.Disposition = src.Disposition
	}
	if dst.Filter == "" {
		dst.Filter = src.Filter
	}
	if dst.FilterAction == "" {
		dst.FilterAction = src.FilterAction
	}
	if dst.SpamScore == "" {
		dst.SpamScore = src.SpamScore
	}
	if dst.FilterRule == "" {
		dst.FilterRule = src.FilterRule
	}
	if dst.FilterID == "" {
		dst.FilterID = src.FilterID
	}
	if dst.DeliveryQueueID == "" {
		dst.DeliveryQueueID = src.DeliveryQueueID
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
	Timestamp       string    `json:"timestamp"`
	QueueID         string    `json:"queue_id"`
	From            string    `json:"from"`
	To              string    `json:"to"`
	Subject         string    `json:"subject"`
	Size            string    `json:"size"`
	SizeBytes       int64     `json:"size_bytes"`
	MessageID       string    `json:"message_id"`
	Status          string    `json:"status"`
	StatusDetail    string    `json:"status_detail"`
	Relay           string    `json:"relay"`
	Client          string    `json:"client"`
	TLS             string    `json:"tls"`
	Disposition     string    `json:"disposition"`
	Filter          string    `json:"filter"`
	FilterAction    string    `json:"filter_action"`
	SpamScore       string    `json:"spam_score"`
	FilterRule      string    `json:"filter_rule"`
	FilterID        string    `json:"filter_id"`
	DeliveryQueueID string    `json:"delivery_queue_id"`
	Lines           []LogLine `json:"lines"`
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
		Timestamp:       ts,
		QueueID:         r.QueueID,
		From:            r.From,
		To:              r.To,
		Subject:         r.Subject,
		Size:            formatSize(r.Size),
		SizeBytes:       r.Size,
		MessageID:       r.MessageID,
		Status:          r.Status,
		StatusDetail:    r.StatusDetail,
		Relay:           r.Relay,
		Client:          r.Client,
		TLS:             r.TLS,
		Disposition:     r.dispositionOrStatus(),
		Filter:          r.Filter,
		FilterAction:    r.FilterAction,
		SpamScore:       r.SpamScore,
		FilterRule:      r.FilterRule,
		FilterID:        r.FilterID,
		DeliveryQueueID: r.DeliveryQueueID,
		Lines:           lines,
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
	visibleTotal := 0
	for i, r := range s.records {
		if r.Subsumed {
			continue // merged into another row; not its own item
		}
		visibleTotal++
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
		RecordsTotal:    visibleTotal,
		RecordsFiltered: total,
		Data:            data,
	}
}

func recordMatchesSearch(r *Record, search string) bool {
	fields := []string{
		r.QueueID, r.From, r.To, r.Subject,
		r.MessageID, r.Status, r.StatusDetail,
		r.Relay, r.Client, r.TLS,
		r.Disposition, r.Filter, r.FilterAction, r.SpamScore, r.FilterRule, r.FilterID,
		r.DeliveryQueueID,
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
	if p.FilterStatus != "" &&
		!strings.EqualFold(r.dispositionOrStatus(), p.FilterStatus) &&
		!strings.EqualFold(r.Status, p.FilterStatus) {
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

// compareByColumn sorts by the visible table column position:
// 0 Time, 1 Queue ID, 2 Client, 3 From, 4 To, 5 Size, 6 Status, 7 Relay.
func compareByColumn(a, b *Record, col int) int {
	switch col {
	case 0:
		return a.Timestamp.Compare(b.Timestamp)
	case 1:
		return strings.Compare(a.QueueID, b.QueueID)
	case 2:
		return strings.Compare(strings.ToLower(a.Client), strings.ToLower(b.Client))
	case 3:
		return strings.Compare(strings.ToLower(a.From), strings.ToLower(b.From))
	case 4:
		return strings.Compare(strings.ToLower(a.To), strings.ToLower(b.To))
	case 5:
		if a.Size < b.Size {
			return -1
		} else if a.Size > b.Size {
			return 1
		}
		return 0
	case 6:
		return strings.Compare(a.dispositionOrStatus(), b.dispositionOrStatus())
	case 7:
		return strings.Compare(a.Relay, b.Relay)
	default:
		return 0
	}
}

func recordToRow(r *Record) []string {
	ts := ""
	if !r.Timestamp.IsZero() {
		ts = r.Timestamp.UTC().Format(time.RFC3339)
	}
	return []string{
		ts,
		r.QueueID,
		r.From,
		r.To,
		formatSize(r.Size),
		r.dispositionOrStatus(), // [5] badge: effective outcome, not raw status
		tooltipDetail(r),        // [6] badge tooltip: raw status + scanner summary
		r.Relay,
		r.Client,
		r.MessageID,
		r.Subject,
		r.TLS,
	}
}

// tooltipDetail builds the hover text for the status badge: the raw Postfix status
// and detail, plus the scanner verdict (score / rule) when one was correlated.
func tooltipDetail(r *Record) string {
	parts := make([]string, 0, 4)
	if r.Status != "" {
		s := r.Status
		if r.StatusDetail != "" {
			s += " " + r.StatusDetail
		}
		parts = append(parts, s)
	} else if r.StatusDetail != "" {
		parts = append(parts, r.StatusDetail)
	}
	if r.SpamScore != "" {
		parts = append(parts, "SA "+r.SpamScore)
	}
	if r.FilterRule != "" {
		parts = append(parts, r.FilterRule)
	}
	return strings.Join(parts, " · ")
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
	Total         int          `json:"total"`
	Sent          int          `json:"sent"`
	Spam          int          `json:"spam"`
	Blocked       int          `json:"blocked"`
	Rejected      int          `json:"rejected"`
	Bounced       int          `json:"bounced"`
	Deferred      int          `json:"deferred"`
	Other         int          `json:"other"`
	TopSenders    []CountItem  `json:"top_senders"`
	TopRecipients []CountItem  `json:"top_recipients"`
	Hourly        []HourlyItem `json:"hourly"`
	Ready         bool         `json:"ready"`
	Status        string       `json:"status"`
}

func (s *Store) Stats() StatsResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := StatsResult{
		Ready:  s.ready,
		Status: s.status,
	}

	senders := make(map[string]int)
	recipients := make(map[string]int)
	hourly := make(map[string]int)

	for i := range s.records {
		r := &s.records[i]
		if r.Subsumed {
			continue // merged into another row
		}
		result.Total++
		// Categorize by the effective disposition so quarantined/blocked mail is
		// counted correctly rather than as "sent".
		switch r.dispositionOrStatus() {
		case "sent", "delivered":
			result.Sent++
		case "spam":
			result.Spam++
		case "blocked", "virus":
			result.Blocked++
		case "rejected", "reject":
			result.Rejected++
		case "bounced":
			result.Bounced++
		case "deferred":
			result.Deferred++
		case "":
			// no status seen yet; don't count
		default:
			result.Other++
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
