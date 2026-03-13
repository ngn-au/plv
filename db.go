package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lib/pq"
)

type DB struct {
	conn *sql.DB
}

func OpenDB(databaseURL string) (*DB, error) {
	conn, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	conn.SetMaxOpenConns(10)
	conn.SetMaxIdleConns(5)
	conn.SetConnMaxLifetime(5 * time.Minute)

	for i := 0; i < 30; i++ {
		if err := conn.Ping(); err == nil {
			break
		}
		log.Printf("waiting for database... (%d/30)", i+1)
		time.Sleep(time.Second)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("database not ready: %w", err)
	}

	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) Migrate() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS mail_records (
			queue_id      TEXT PRIMARY KEY,
			timestamp     TIMESTAMPTZ,
			from_addr     TEXT NOT NULL DEFAULT '',
			to_addr       TEXT NOT NULL DEFAULT '',
			subject       TEXT NOT NULL DEFAULT '',
			size          BIGINT NOT NULL DEFAULT 0,
			message_id    TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT '',
			status_detail TEXT NOT NULL DEFAULT '',
			relay         TEXT NOT NULL DEFAULT '',
			client        TEXT NOT NULL DEFAULT '',
			tls           TEXT NOT NULL DEFAULT '',
			raw_lines     TEXT[] NOT NULL DEFAULT '{}'
		);
		CREATE INDEX IF NOT EXISTS idx_mail_records_timestamp ON mail_records(timestamp DESC);
		CREATE INDEX IF NOT EXISTS idx_mail_records_from ON mail_records(from_addr);
		CREATE INDEX IF NOT EXISTS idx_mail_records_to ON mail_records(to_addr);
		CREATE INDEX IF NOT EXISTS idx_mail_records_status ON mail_records(status);
	`)
	return err
}

func (db *DB) LoadAll() ([]Record, error) {
	rows, err := db.conn.Query(`
		SELECT queue_id, timestamp, from_addr, to_addr, subject, size,
		       message_id, status, status_detail, relay, client, tls, raw_lines
		FROM mail_records ORDER BY timestamp DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		var r Record
		var ts sql.NullTime
		var rawLines []string
		err := rows.Scan(
			&r.QueueID, &ts, &r.From, &r.To, &r.Subject, &r.Size,
			&r.MessageID, &r.Status, &r.StatusDetail, &r.Relay, &r.Client, &r.TLS,
			pq.Array(&rawLines),
		)
		if err != nil {
			return nil, err
		}
		if ts.Valid {
			r.Timestamp = ts.Time
		}
		r.RawLines = rawLines
		records = append(records, r)
	}
	return records, rows.Err()
}

// UpsertRecords persists records to PostgreSQL in batches.
// Each record is the fully-merged state from the in-memory store.
func (db *DB) UpsertRecords(records []Record) error {
	const batchSize = 500
	for i := 0; i < len(records); i += batchSize {
		end := i + batchSize
		if end > len(records) {
			end = len(records)
		}
		if err := db.upsertBatch(records[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) upsertBatch(records []Record) error {
	if len(records) == 0 {
		return nil
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(pq.CopyIn("mail_records",
		"queue_id", "timestamp", "from_addr", "to_addr", "subject", "size",
		"message_id", "status", "status_detail", "relay", "client", "tls", "raw_lines",
	))
	if err != nil {
		// COPY not available or table already has data — fall back to upsert
		return db.upsertBatchFallback(records)
	}
	stmt.Close()
	tx.Rollback()

	// Use multi-row upsert for correctness (handles conflicts)
	return db.upsertBatchFallback(records)
}

func (db *DB) upsertBatchFallback(records []Record) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO mail_records (queue_id, timestamp, from_addr, to_addr, subject, size,
			message_id, status, status_detail, relay, client, tls, raw_lines)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (queue_id) DO UPDATE SET
			timestamp     = EXCLUDED.timestamp,
			from_addr     = EXCLUDED.from_addr,
			to_addr       = EXCLUDED.to_addr,
			subject       = EXCLUDED.subject,
			size          = EXCLUDED.size,
			message_id    = EXCLUDED.message_id,
			status        = EXCLUDED.status,
			status_detail = EXCLUDED.status_detail,
			relay         = EXCLUDED.relay,
			client        = EXCLUDED.client,
			tls           = EXCLUDED.tls,
			raw_lines     = EXCLUDED.raw_lines
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range records {
		var ts *time.Time
		if !r.Timestamp.IsZero() {
			t := r.Timestamp
			ts = &t
		}
		rawLines := r.RawLines
		if rawLines == nil {
			rawLines = []string{}
		}
		_, err := stmt.Exec(
			r.QueueID, ts, r.From, r.To, r.Subject, r.Size,
			r.MessageID, r.Status, r.StatusDetail, r.Relay, r.Client, r.TLS,
			pq.Array(rawLines),
		)
		if err != nil {
			return fmt.Errorf("upsert %s: %w", r.QueueID, err)
		}
	}

	return tx.Commit()
}

func (db *DB) RecordCount() (int, error) {
	var count int
	err := db.conn.QueryRow("SELECT COUNT(*) FROM mail_records").Scan(&count)
	return count, err
}

// DeleteOlderThan removes records older than the given duration.
func (db *DB) DeleteOlderThan(d time.Duration) (int64, error) {
	cutoff := time.Now().Add(-d)
	result, err := db.conn.Exec("DELETE FROM mail_records WHERE timestamp < $1", cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// buildSearchQuery generates a SQL query with filters for server-side search.
// This is unused for now (search is done in-memory) but available for future use.
func buildSearchQuery(p SearchParams) (string, []interface{}) {
	var conditions []string
	var args []interface{}
	n := 1

	if p.SearchTerm != "" {
		conditions = append(conditions, fmt.Sprintf(`(
			queue_id ILIKE $%d OR from_addr ILIKE $%d OR to_addr ILIKE $%d OR
			subject ILIKE $%d OR message_id ILIKE $%d OR status ILIKE $%d OR
			relay ILIKE $%d OR client ILIKE $%d
		)`, n, n, n, n, n, n, n, n))
		args = append(args, "%"+p.SearchTerm+"%")
		n++
	}
	if p.FilterFrom != "" {
		conditions = append(conditions, fmt.Sprintf("from_addr ILIKE $%d", n))
		args = append(args, "%"+p.FilterFrom+"%")
		n++
	}
	if p.FilterTo != "" {
		conditions = append(conditions, fmt.Sprintf("to_addr ILIKE $%d", n))
		args = append(args, "%"+p.FilterTo+"%")
		n++
	}
	if p.FilterClient != "" {
		conditions = append(conditions, fmt.Sprintf("client ILIKE $%d", n))
		args = append(args, "%"+p.FilterClient+"%")
		n++
	}
	if p.FilterRelay != "" {
		conditions = append(conditions, fmt.Sprintf("relay ILIKE $%d", n))
		args = append(args, "%"+p.FilterRelay+"%")
		n++
	}
	if p.FilterStatus != "" {
		conditions = append(conditions, fmt.Sprintf("status ILIKE $%d", n))
		args = append(args, p.FilterStatus)
		n++
	}
	if p.FilterQueueID != "" {
		conditions = append(conditions, fmt.Sprintf("queue_id ILIKE $%d", n))
		args = append(args, "%"+p.FilterQueueID+"%")
		n++
	}
	if p.FilterTLS == "yes" {
		conditions = append(conditions, "tls != ''")
	} else if p.FilterTLS == "no" {
		conditions = append(conditions, "tls = ''")
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	return where, args
}
