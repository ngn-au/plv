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
	Timestamp time.Time
	// Origin is the forwarding server's name (its client-cert CN, stamped by the
	// receiver); empty in standalone mode. Part of the identity: (Origin, QueueID).
	Origin       string
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
	hasOrigins    bool                    // any record carries a non-empty Origin (distributed mode)
	retentionDays int                     // configured RETENTION_DAYS (0 = disabled); surfaced in Stats
	pcfg          *PostfixConf            // local Postfix config (standalone / receiver fallback); may be nil
	pcfgByOrigin  map[string]*PostfixConf // per-forwarder config (distributed), keyed by origin/CN
	db            *DB
	sink          RecordSink // forwarder mode: ship merged snapshots to a receiver
}

// SetRetentionDays records the configured retention window so the UI can show it.
func (s *Store) SetRetentionDays(days int) {
	s.mu.Lock()
	s.retentionDays = days
	s.mu.Unlock()
}

// SetPostfixConf swaps in the latest Postfix-derived direction facts (mynetworks / local
// domains). Safe to call live from the config watcher.
func (s *Store) SetPostfixConf(pc *PostfixConf) {
	s.mu.Lock()
	s.pcfg = pc
	s.mu.Unlock()
}

// SetPostfixConfForOrigin records a forwarder's config under its origin/CN (distributed
// mode), so direction for that server's mail uses its own mynetworks / local domains. It
// reports whether the config actually changed, so the heartbeat (which re-sends the same
// config) doesn't spam logs.
func (s *Store) SetPostfixConfForOrigin(origin string, pc *PostfixConf) bool {
	if origin == "" || pc == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pcfgByOrigin == nil {
		s.pcfgByOrigin = map[string]*PostfixConf{}
	}
	if old := s.pcfgByOrigin[origin]; old != nil && old.signature() == pc.signature() {
		return false
	}
	s.pcfgByOrigin[origin] = pc
	return true
}

// confFor returns the Postfix config to use for a record's origin: the forwarder's own
// config in distributed mode, else the local/standalone config. Caller holds the lock.
func (s *Store) confFor(origin string) *PostfixConf {
	if origin != "" && s.pcfgByOrigin != nil {
		if pc := s.pcfgByOrigin[origin]; pc != nil {
			return pc
		}
	}
	return s.pcfg
}

// ServerConfInfo is the PLV-relevant Postfix settings of one server, for the Servers page.
type ServerConfInfo struct {
	Name         string   `json:"name"`
	LocalDomains []string `json:"local_domains"`
	Networks     []string `json:"networks"`
}

// ServerConfs returns the derived Postfix settings per server (one entry in standalone;
// nil when no config is loaded). Only the two settings PLV actually uses for direction —
// mynetworks (trusted networks) and the local/hosted domains — are surfaced.
func (s *Store) ServerConfs() []ServerConfInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := func(origin string, pc *PostfixConf) ServerConfInfo {
		doms := make([]string, 0, len(pc.LocalDomains))
		for d := range pc.LocalDomains {
			doms = append(doms, d)
		}
		sort.Strings(doms)
		nets := make([]string, 0, len(pc.Networks))
		for _, n := range pc.Networks {
			nets = append(nets, n.String())
		}
		sort.Strings(nets)
		name := origin
		if name == "" {
			name = pc.Hostname
		}
		if name == "" {
			name = "this server"
		}
		return ServerConfInfo{Name: name, LocalDomains: doms, Networks: nets}
	}
	var out []ServerConfInfo
	origins := make([]string, 0, len(s.pcfgByOrigin))
	for o := range s.pcfgByOrigin {
		origins = append(origins, o)
	}
	sort.Strings(origins)
	for _, o := range origins {
		out = append(out, info(o, s.pcfgByOrigin[o]))
	}
	if s.pcfg != nil {
		out = append(out, info("", s.pcfg))
	}
	return out
}

// RecordSink receives fully-merged record snapshots. In forwarder mode the store
// hands snapshots to a sink that batches and ships them to the receiver, in place
// of writing to a database. Enqueue must not block.
type RecordSink interface {
	Enqueue([]Record)
}

// recKey is a record's in-memory identity. Two servers can emit the same Postfix
// queue id, so the origin (forwarding server) is part of the key.
func recKey(origin, qid string) string { return origin + "\x00" + qid }

// logSafe renders a value for logging with line breaks removed, so attacker-influenced
// data — forwarded record fields, a client-cert CN — can't forge or split log lines.
func logSafe(v interface{}) string {
	s := fmt.Sprintf("%v", v)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
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

// SetSink installs a forwarder sink. Records the store merges are then shipped to
// it (in addition to any DB). Call before parsing begins.
func (s *Store) SetSink(sink RecordSink) { s.sink = sink }

// persisting reports whether merged snapshots need to be captured (for a DB and/or
// a forwarder sink).
func (s *Store) persisting() bool { return s.db != nil || s.sink != nil }

// flush hands merged snapshots to the DB and/or the forwarder sink. Called without
// the store lock held.
func (s *Store) flush(records []Record) {
	if len(records) == 0 {
		return
	}
	if s.db != nil {
		if err := s.db.UpsertRecords(records); err != nil {
			log.Printf("db persist error: %v", err)
		}
	}
	if s.sink != nil {
		s.sink.Enqueue(records)
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
		if r.Origin != "" {
			s.hasOrigins = true
		}
		if r.Subsumed {
			continue // merged into another item; not loaded as its own row
		}
		key := recKey(r.Origin, r.QueueID)
		if _, exists := s.byQueueID[key]; !exists {
			idx := len(s.records)
			s.byQueueID[key] = idx
			s.records = append(s.records, r)
			// Restore the delivery link so the outbound queue id resolves to this
			// merged primary for search/detail.
			if r.DeliveryQueueID != "" {
				dkey := recKey(r.Origin, r.DeliveryQueueID)
				s.byDelivery[dkey] = idx
				if _, ok := s.byQueueID[dkey]; !ok {
					s.byQueueID[dkey] = idx
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
	persist := s.persisting()
	var toUpsert []Record
	for _, r := range records {
		if r.Origin != "" {
			s.hasOrigins = true
		}
		key := recKey(r.Origin, r.QueueID)
		// (1) A re-injected outbound leg whose inbound primary we already have:
		// fold the final delivery into the primary instead of adding a new row.
		if pIdx, ok := s.byDelivery[key]; ok && pIdx < len(s.records) {
			mergeDeliveryLeg(&s.records[pIdx], &r)
			s.byQueueID[key] = pIdx // resolve the outbound id to the primary
			if persist {
				toUpsert = append(toUpsert, snapshotRecord(&s.records[pIdx]))
			}
			continue
		}

		// (2) Same queue id seen again: merge incrementally (as before).
		if idx, exists := s.byQueueID[key]; exists && !s.records[idx].Subsumed {
			s.mergeRecord(&s.records[idx], &r)
			if r.DeliveryQueueID != "" {
				s.registerDelivery(idx, &toUpsert)
			}
			s.applyPendingRspamd(idx)
			if persist {
				toUpsert = append(toUpsert, snapshotRecord(&s.records[idx]))
			}
			continue
		}

		// (3) New record.
		idx := len(s.records)
		s.byQueueID[key] = idx
		s.records = append(s.records, r)
		if r.DeliveryQueueID != "" {
			s.registerDelivery(idx, &toUpsert)
		}
		s.applyPendingRspamd(idx)
		if persist {
			toUpsert = append(toUpsert, snapshotRecord(&s.records[idx]))
		}
	}
	s.mu.Unlock()

	s.flush(toUpsert)
}

// IngestSnapshots upserts already-merged records forwarded from another instance.
// origin (the forwarder's verified client-cert CN) is stamped authoritatively,
// overriding whatever the payload claimed. No leg-merge/correlation runs here —
// that happened on the forwarder; this mirrors the DB-load reconstruction
// (subsumed legs are persisted but not listed; the delivery link is restored from
// the primary). Returns the number of records accepted.
func (s *Store) IngestSnapshots(origin string, records []Record) int {
	s.mu.Lock()
	var toPersist []Record
	accepted := 0
	for i := range records {
		r := records[i]
		r.Origin = origin
		if origin != "" {
			s.hasOrigins = true
		}
		accepted++
		if s.db != nil {
			toPersist = append(toPersist, snapshotRecord(&r))
		}
		if r.Subsumed {
			continue // merged into a primary on the forwarder; not its own row
		}
		key := recKey(origin, r.QueueID)
		if idx, ok := s.byQueueID[key]; ok && idx < len(s.records) &&
			s.records[idx].Origin == origin && s.records[idx].QueueID == r.QueueID && !s.records[idx].Subsumed {
			s.records[idx] = r // replace with the latest snapshot
			s.restoreDeliveryLink(idx)
		} else {
			idx := len(s.records)
			s.byQueueID[key] = idx
			s.records = append(s.records, r)
			s.restoreDeliveryLink(idx)
		}
	}
	s.mu.Unlock()

	if s.db != nil && len(toPersist) > 0 {
		if err := s.db.UpsertRecords(toPersist); err != nil {
			log.Printf("db persist error: %s", logSafe(err)) // forwarded data may taint the error
		}
	}
	return accepted
}

// restoreDeliveryLink makes the record's onward (post-filter) queue id resolve to
// the primary at idx, so either id is searchable. Caller holds s.mu.
func (s *Store) restoreDeliveryLink(idx int) {
	r := &s.records[idx]
	if r.DeliveryQueueID == "" {
		return
	}
	dkey := recKey(r.Origin, r.DeliveryQueueID)
	s.byDelivery[dkey] = idx
	if _, ok := s.byQueueID[dkey]; !ok {
		s.byQueueID[dkey] = idx
	}
}

// ApplyRspamdVerdict correlates an rspamd verdict to its mail record by queue id.
// If the record exists it is enriched immediately; otherwise the verdict is held
// (briefly) until the record is added. Returns true when it matched a record.
func (s *Store) ApplyRspamdVerdict(qid string, v rspamdVerdict) bool {
	s.mu.Lock()
	// rspamd correlation is always local to the parsing instance (origin is empty).
	idx, ok := s.byQueueID[recKey("", qid)]
	if ok && idx < len(s.records) && !s.records[idx].Subsumed {
		enrichRecordWithRspamd(&s.records[idx], v)
		var snaps []Record
		if s.persisting() {
			snaps = []Record{snapshotRecord(&s.records[idx])}
		}
		s.mu.Unlock()
		s.flush(snaps)
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
	dkey := recKey(s.records[idx].Origin, s.records[idx].DeliveryQueueID)
	s.byDelivery[dkey] = idx
	if oIdx, ok := s.byQueueID[dkey]; ok && oIdx != idx && !s.records[oIdx].Subsumed {
		mergeDeliveryLeg(&s.records[idx], &s.records[oIdx])
		s.records[oIdx].Subsumed = true
		s.byQueueID[dkey] = idx
		if s.persisting() {
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
	Timestamp       string        `json:"timestamp"`
	Origin          string        `json:"origin"`
	QueueID         string        `json:"queue_id"`
	From            string        `json:"from"`
	To              string        `json:"to"`
	Subject         string        `json:"subject"`
	Size            string        `json:"size"`
	SizeBytes       int64         `json:"size_bytes"`
	MessageID       string        `json:"message_id"`
	Status          string        `json:"status"`
	StatusDetail    string        `json:"status_detail"`
	Relay           string        `json:"relay"`
	Client          string        `json:"client"`
	TLS             string        `json:"tls"`
	Disposition     string        `json:"disposition"`
	Filter          string        `json:"filter"`
	FilterAction    string        `json:"filter_action"`
	SpamScore       string        `json:"spam_score"`
	FilterRule      string        `json:"filter_rule"`
	FilterID        string        `json:"filter_id"`
	DeliveryQueueID string        `json:"delivery_queue_id"`
	Direction       string        `json:"direction"` // inbound/outbound/internal/relayed (mynetworks + local-domain aware)
	NDR             bool          `json:"ndr"`
	Related         []RelatedItem `json:"related,omitempty"`
	Lines           []LogLine     `json:"lines"`
}

type LogLine struct {
	Timestamp string `json:"timestamp"`
	Raw       string `json:"raw"`
}

// RelatedItem is the same message (matched by Message-ID) seen on another server —
// surfaced as a cross-node link, never merged into the record.
type RelatedItem struct {
	Origin      string `json:"origin"`
	QueueID     string `json:"queue_id"`
	Disposition string `json:"disposition"`
	Timestamp   string `json:"timestamp"`
}

func (s *Store) GetDetail(origin, queueID string) *RecordDetail {
	s.mu.RLock()
	defer s.mu.RUnlock()

	idx, ok := s.byQueueID[recKey(origin, queueID)]
	if !ok {
		return nil
	}
	r := &s.records[idx]

	ts := ""
	if !r.Timestamp.IsZero() {
		ts = r.Timestamp.UTC().Format(time.RFC3339)
	}

	// rspamd lines are zone-less; parse them in the offset of the record's Postfix lines.
	// Derive that from a raw line's RFC3339 text (always carries the offset, even after
	// forwarding/merge) rather than r.Timestamp, whose zone may be normalised to UTC.
	logLoc := time.Local
	for _, raw := range r.RawLines {
		if loc := logLineLocation(raw); loc != nil {
			logLoc = loc
			break
		}
	}

	lines := make([]LogLine, 0, len(r.RawLines))
	for _, raw := range r.RawLines {
		lineTs := extractTimestamp(raw)
		if lineTs.IsZero() {
			lineTs = extractRspamdTimeIn(raw, logLoc) // zone-less rspamd line → log's offset
		}
		fmtTs := ""
		if !lineTs.IsZero() {
			fmtTs = lineTs.UTC().Format(time.RFC3339)
		}
		lines = append(lines, LogLine{Timestamp: fmtTs, Raw: raw})
	}

	// Cross-node correlation AND direction in a single pass. Both are keyed on Message-ID:
	// the related links are this message's other legs, and the direction aggregates those
	// same legs' signals. Done inline here rather than via directionResolver() — which
	// rebuilds the global six-map signal index over EVERY record on each call — so a detail
	// open stays O(this message's legs) for the costly IP/CIDR predicates instead of
	// O(all records). GetDetail runs on every modal open (and once per related leg), so the
	// old per-call full scan was the bulk of the open latency.
	pc := s.confFor(r.Origin)
	in, out, local, relay := legReceivedPublicUnauth(r, pc), legSendsPublic(r, pc), deliversLocal(r), isRelayLeg(r, pc)
	fromLocal, toLocal := pc.isLocalDomain(r.From), pc.isLocalDomain(r.To)
	var related []RelatedItem
	if r.MessageID != "" {
		// With a Message-ID the direction aggregates across the message's non-subsumed legs
		// (matching directionResolver), so reset and OR every leg — including this one — in.
		in, out, local, relay, fromLocal, toLocal = false, false, false, false, false, false
		for i := range s.records {
			o := &s.records[i]
			if o.Subsumed || o.MessageID != r.MessageID {
				continue
			}
			opc := s.confFor(o.Origin)
			in = in || legReceivedPublicUnauth(o, opc)
			out = out || legSendsPublic(o, opc)
			local = local || deliversLocal(o)
			relay = relay || isRelayLeg(o, opc)
			fromLocal = fromLocal || opc.isLocalDomain(o.From)
			toLocal = toLocal || opc.isLocalDomain(o.To)
			if o.Origin == r.Origin && o.QueueID == r.QueueID {
				continue // self: aggregated above, but it isn't a "related" link
			}
			ots := ""
			if !o.Timestamp.IsZero() {
				ots = o.Timestamp.UTC().Format(time.RFC3339)
			}
			related = append(related, RelatedItem{
				Origin: o.Origin, QueueID: o.QueueID,
				Disposition: o.dispositionOrStatus(), Timestamp: ots,
			})
		}
	}

	direction := classifyDirection(in, out, local, relay, fromLocal, toLocal) // mail-path source of truth

	return &RecordDetail{
		Timestamp:       ts,
		Origin:          r.Origin,
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
		Direction:       direction,
		NDR:             isNDR(r),
		Related:         related,
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
		newIndex[recKey(r.Origin, r.QueueID)] = len(kept)
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

// HasOrigins reports whether any record carries a forwarding server name, i.e.
// the instance is showing amalgamated data and the UI should show the Server column.
func (s *Store) HasOrigins() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasOrigins
}

// Suggest returns distinct values of a field (from|to|subject|client|relay),
// most-frequent first, capped at limit — for live autocomplete on the filters.
func (s *Store) Suggest(field string, limit int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := make(map[string]int)
	for i := range s.records {
		r := &s.records[i]
		if r.Subsumed {
			continue
		}
		var v string
		switch field {
		case "from":
			v = r.From
		case "to":
			v = r.To
		case "subject":
			v = r.Subject
		case "client":
			v = r.Client
		case "relay":
			v = r.Relay
		case "origin", "server":
			v = r.Origin
		}
		if v != "" {
			counts[v]++
		}
	}
	items := make([]string, 0, len(counts))
	for v := range counts {
		items = append(items, v)
	}
	sort.Slice(items, func(a, b int) bool {
		if counts[items[a]] != counts[items[b]] {
			return counts[items[a]] > counts[items[b]]
		}
		return items[a] < items[b]
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

// Count returns the number of records held (including subsumed legs).
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}

type SearchParams struct {
	Draw       int
	Start      int
	Length     int
	SearchTerm string
	OrderCol   int
	OrderDir   string

	FilterFrom      string
	FilterTo        string
	FilterSubject   string
	FilterClient    string
	FilterRelay     string
	FilterStatus    string
	FilterQueueID   string
	FilterTLS       string // "yes", "no", or "" (any)
	FilterNDR       string // "yes" to show only non-delivery reports
	FilterServer    string // comma-separated server names to include ("" = all)
	FilterDirection string // "inbound"|"outbound"|"internal" or "" (any)
	Group           bool   // cluster correlated rows (same Message-ID) together
}

type SearchResult struct {
	Draw            int        `json:"draw"`
	RecordsTotal    int        `json:"recordsTotal"`
	RecordsFiltered int        `json:"recordsFiltered"`
	Data            [][]string `json:"data"`
	// When grouping is on, these report whether the page's first/last correlation
	// group spills onto the adjacent page, so the UI can run the rail off the edge.
	ContinuesAbove bool `json:"continues_above,omitempty"`
	ContinuesBelow bool `json:"continues_below,omitempty"`
}

func (s *Store) Search(p SearchParams) SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	direction := s.directionResolver()

	var filtered []int
	visibleTotal := 0
	for i := range s.records {
		r := &s.records[i]
		if r.Subsumed {
			continue // merged into another row; not its own item
		}
		visibleTotal++
		if !recordMatches(r, &p) {
			continue
		}
		if p.FilterDirection != "" && !strings.EqualFold(direction(r), p.FilterDirection) {
			continue
		}
		filtered = append(filtered, i)
	}

	groupKey := func(r *Record) string {
		if r.MessageID != "" {
			return "m:" + r.MessageID
		}
		return "q:" + recKey(r.Origin, r.QueueID)
	}
	// Always sort by the chosen column + direction first, so column sorting works
	// (this is the global order the client asked for). Column index 16 is the derived
	// Direction column, which isn't a Record field — compare the resolved values.
	sort.SliceStable(filtered, func(a, b int) bool {
		ra, rb := &s.records[filtered[a]], &s.records[filtered[b]]
		var cmp int
		if p.OrderCol == 16 {
			cmp = strings.Compare(direction(ra), direction(rb))
		} else {
			cmp = compareByColumn(ra, rb, p.OrderCol)
		}
		if p.OrderDir == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})
	if p.Group {
		// Cluster correlated rows together WITHOUT disturbing that order: anchor each
		// group at its best-ranked row in the sorted list, then stable-sort by that
		// rank. Within-group order then stays identical to the global order — no
		// reversal — and it follows whatever column/direction is active.
		rank := make(map[string]int, len(filtered))
		for pos, idx := range filtered {
			k := groupKey(&s.records[idx])
			if _, seen := rank[k]; !seen {
				rank[k] = pos
			}
		}
		sort.SliceStable(filtered, func(a, b int) bool {
			return rank[groupKey(&s.records[filtered[a]])] < rank[groupKey(&s.records[filtered[b]])]
		})
	}

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

	// Flag rows whose Message-ID appears under more than one origin (cross-node).
	midOrigins := make(map[string]map[string]struct{})
	for i := range s.records {
		rr := &s.records[i]
		if rr.Subsumed || rr.MessageID == "" {
			continue
		}
		set := midOrigins[rr.MessageID]
		if set == nil {
			set = make(map[string]struct{})
			midOrigins[rr.MessageID] = set
		}
		set[rr.Origin] = struct{}{}
	}

	data := make([][]string, len(page))
	for i, idx := range page {
		row := recordToRow(&s.records[idx])
		flag := ""
		if mid := s.records[idx].MessageID; mid != "" && len(midOrigins[mid]) > 1 {
			flag = "1"
		}
		ndr := ""
		if isNDR(&s.records[idx]) {
			ndr = "1"
		}
		data[i] = append(row, flag, ndr, groupKey(&s.records[idx]), direction(&s.records[idx]))
	}

	// When grouping, note whether the page's edge groups continue onto an adjacent
	// page, so the UI can render the rail running off the top/bottom edge.
	var contAbove, contBelow bool
	if p.Group && end > start {
		if start > 0 && groupKey(&s.records[filtered[start-1]]) == groupKey(&s.records[filtered[start]]) {
			contAbove = true
		}
		if end < total && groupKey(&s.records[filtered[end]]) == groupKey(&s.records[filtered[end-1]]) {
			contBelow = true
		}
	}

	return SearchResult{
		Draw:            p.Draw,
		RecordsTotal:    visibleTotal,
		RecordsFiltered: total,
		Data:            data,
		ContinuesAbove:  contAbove,
		ContinuesBelow:  contBelow,
	}
}

// recordMatches reports whether a record passes the active search + advanced
// filters. Shared by Search (table) and Stats (graphs) so both reflect the filters.
func recordMatches(r *Record, p *SearchParams) bool {
	if term := strings.ToLower(p.SearchTerm); term != "" && !recordMatchesSearch(r, term) {
		return false
	}
	if p.FilterServer != "" && !serverSelected(r.Origin, p.FilterServer) {
		return false
	}
	if p.FilterFrom != "" || p.FilterTo != "" || p.FilterSubject != "" || p.FilterClient != "" ||
		p.FilterRelay != "" || p.FilterStatus != "" || p.FilterQueueID != "" || p.FilterTLS != "" || p.FilterNDR != "" {
		if !recordMatchesAdvanced(r, p) {
			return false
		}
	}
	return true
}

// serverSelected reports whether origin is in the comma-separated allow-list — used
// by the table's server multi-select. Correlation/grouping is unaffected (it keys on
// Message-ID), so this only narrows which rows are listed.
func serverSelected(origin, csv string) bool {
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" && strings.EqualFold(origin, s) {
			return true
		}
	}
	return false
}

// recordMatchesSearch does intuitive multi-term matching: the query is split on
// whitespace and EVERY term must appear somewhere in the record (case-insensitive),
// so "spam example.com" narrows rather than widens. search is already lower-cased.
func recordMatchesSearch(r *Record, search string) bool {
	terms := strings.Fields(search)
	if len(terms) == 0 {
		return true
	}
	blob := strings.ToLower(strings.Join([]string{
		r.Origin, r.QueueID, r.From, r.To, r.Subject, r.MessageID, r.Status, r.StatusDetail,
		r.Relay, r.Client, r.TLS, r.Disposition, r.Filter, r.FilterAction, r.SpamScore,
		r.FilterRule, r.FilterID, r.DeliveryQueueID,
	}, " ")) + " " + r.Timestamp.UTC().Format("2006-01-02 15:04:05")
	for _, t := range terms {
		if !strings.Contains(blob, t) {
			return false
		}
	}
	return true
}

func recordMatchesAdvanced(r *Record, p *SearchParams) bool {
	if p.FilterFrom != "" && !strings.Contains(strings.ToLower(r.From), strings.ToLower(p.FilterFrom)) {
		return false
	}
	if p.FilterTo != "" && !strings.Contains(strings.ToLower(r.To), strings.ToLower(p.FilterTo)) {
		return false
	}
	if p.FilterSubject != "" && !strings.Contains(strings.ToLower(r.Subject), strings.ToLower(p.FilterSubject)) {
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
	if p.FilterNDR == "yes" && !isNDR(r) {
		return false
	}
	return true
}

// compareByColumn sorts by the data-array index the client sends
// (DataTables columns[i][data]), which is stable regardless of column display
// order. Indices match recordToRow: 0 Time, 1 Queue ID, 2 From, 3 To, 4 Size,
// 5 Status, 7 Relay, 8 Client, 12 Server (Origin).
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
		return strings.Compare(a.dispositionOrStatus(), b.dispositionOrStatus())
	case 7:
		return strings.Compare(a.Relay, b.Relay)
	case 8:
		return strings.Compare(strings.ToLower(a.Client), strings.ToLower(b.Client))
	case 10:
		return strings.Compare(strings.ToLower(a.Subject), strings.ToLower(b.Subject))
	case 12:
		return strings.Compare(strings.ToLower(a.Origin), strings.ToLower(b.Origin))
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
		r.Origin, // [12] forwarding server (Server column); empty in standalone
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

// isNDR reports whether a record is a non-delivery report / bounce. Postfix logs a
// null envelope sender (from=<>) for system-generated bounce/DSN messages, which is
// why their From/Subject columns are typically empty.
func isNDR(r *Record) bool {
	// A non-delivery report is a delivery itself (a bounce/DSN), recognised by its
	// null return-path (from=<>). Exclude content-scanner / reject verdicts: spam and
	// junk routinely use a null sender but are NOT non-delivery reports.
	switch r.dispositionOrStatus() {
	case "spam", "virus", "blocked", "rejected", "reject":
		return false
	}
	for _, l := range r.RawLines {
		if strings.Contains(l, "from=<>") {
			return true
		}
	}
	return false
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
	NDR           int          `json:"ndr"`
	Other         int          `json:"other"`
	Inbound       int          `json:"inbound"`
	Outbound      int          `json:"outbound"`
	Internal      int          `json:"internal"`
	Relayed       int          `json:"relayed"`
	TopSenders    []CountItem  `json:"top_senders"`
	TopRecipients []CountItem  `json:"top_recipients"`
	Hourly        []HourlyItem `json:"hourly"`
	Earliest      string       `json:"earliest"`       // RFC3339 of the oldest message (for the "N days" header)
	RetentionDays int          `json:"retention_days"` // configured RETENTION_DAYS, 0 = disabled
	Ready         bool         `json:"ready"`
	Status        string       `json:"status"`
}

func (s *Store) Stats(p SearchParams) StatsResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := StatsResult{
		Ready:         s.ready,
		Status:        s.status,
		RetentionDays: s.retentionDays,
	}

	senders := make(map[string]int)
	recipients := make(map[string]int)
	hourly := make(map[string]int)
	direction := s.directionResolver()
	var earliest time.Time

	for i := range s.records {
		r := &s.records[i]
		if r.Subsumed {
			continue // merged into another row
		}
		if !recordMatches(r, &p) {
			continue // graphs reflect the active filters
		}
		// Direction tallies (for the direction cards) count every match within the
		// non-direction filters — independent of the direction filter, so all four
		// counts stay visible while one is selected.
		switch direction(r) {
		case "inbound":
			result.Inbound++
		case "outbound":
			result.Outbound++
		case "internal":
			result.Internal++
		case "relayed":
			result.Relayed++
		}
		if p.FilterDirection != "" && !strings.EqualFold(direction(r), p.FilterDirection) {
			continue
		}
		if !r.Timestamp.IsZero() && (earliest.IsZero() || r.Timestamp.Before(earliest)) {
			earliest = r.Timestamp
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
		if isNDR(r) {
			result.NDR++
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
	if !earliest.IsZero() {
		result.Earliest = earliest.UTC().Format(time.RFC3339)
	}
	return result
}

func topN(counts map[string]int, n int) []CountItem {
	items := make([]CountItem, 0, len(counts))
	for name, count := range counts {
		items = append(items, CountItem{Name: name, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Name < items[j].Name // stable tiebreak so equal counts don't reshuffle
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
