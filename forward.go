package main

// Forwarder side of the distributed mode. The normal parse pipeline runs locally
// (no DB); merged record snapshots are handed to this sink, which batches and
// POSTs them to the receiver over mTLS. A per-logfile checkpoint of receiver-acked
// offsets lets a clean restart resume instead of re-shipping history; on anything
// ambiguous (missing checkpoint or a rotated/shrunk log) we re-parse fully and let
// the receiver dedupe by (origin, queue-id).

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const forwardStateFile = "forward-state.json"

type forwardState struct {
	Mail   int64 `json:"mail_offset"`
	Rspamd int64 `json:"rspamd_offset"`
}

func loadForwardState(dataDir string) forwardState {
	var st forwardState
	if b, err := os.ReadFile(dataPath(dataDir, forwardStateFile)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// Forwarder is a RecordSink that ships snapshots to a receiver.
type Forwarder struct {
	url     string
	dataDir string
	client  *http.Client

	mu    sync.Mutex
	queue []Record
	wake  chan struct{}
	conf  *ConfPayload // this node's Postfix-derived direction facts, shipped with batches

	mailOff   func() int64
	rspamdOff func() int64
}

// SetConf updates the Postfix-derived config shipped to the receiver (called live by the
// config watcher). It is included on every batch so the receiver always has the latest.
func (f *Forwarder) SetConf(pc *PostfixConf) {
	f.mu.Lock()
	f.conf = pc.payload()
	f.mu.Unlock()
}

func NewForwarder(receiverURL, dataDir string) (*Forwarder, error) {
	tlsCfg, err := clientTLSConfig(dataDir)
	if err != nil {
		return nil, fmt.Errorf("load client certs from %s: %w", dataDir, err)
	}
	return &Forwarder{
		url:     strings.TrimRight(receiverURL, "/"),
		dataDir: dataDir,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
		wake: make(chan struct{}, 1),
	}, nil
}

// SetCheckpointSources wires the live tail offsets used for the resume checkpoint.
func (f *Forwarder) SetCheckpointSources(mailOff, rspamdOff func() int64) {
	f.mailOff = mailOff
	f.rspamdOff = rspamdOff
}

// Enqueue implements RecordSink.
func (f *Forwarder) Enqueue(records []Record) {
	if len(records) == 0 {
		return
	}
	f.mu.Lock()
	f.queue = append(f.queue, records...)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *Forwarder) Run(ctx context.Context) {
	backoff := time.Second
	const heartbeat = 20 * time.Second // re-ship the config even with no new records
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	log.Printf("forwarder: shipping to %s", f.url)
	var lastSend time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.wake:
		case <-tick.C:
		}

		// Capture offsets BEFORE draining so the checkpoint we persist on ack is a
		// conservative lower bound (every record with offset <= cp is in this batch).
		cp := f.checkpoint()

		f.mu.Lock()
		batch := f.queue
		f.queue = nil
		haveConf := f.conf != nil
		f.mu.Unlock()

		// Idle: nothing to ship and the config heartbeat isn't due yet. The heartbeat
		// sends an empty batch carrying the config so the receiver always has each
		// forwarder's mynetworks/domains — and recovers them after a receiver restart,
		// even when the logs are static and no records are flowing.
		if len(batch) == 0 && (!haveConf || time.Since(lastSend) < heartbeat) {
			continue
		}

		if err := f.send(ctx, batch, cp); err != nil {
			log.Printf("forwarder: send failed: %v (retrying in %s)", err, backoff)
			if len(batch) > 0 {
				f.mu.Lock()
				f.queue = append(batch, f.queue...) // re-queue records, preserve order
				f.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		lastSend = time.Now()
		backoff = time.Second
		if len(batch) > 0 {
			log.Printf("forwarder: shipped %d records (mail offset %d)", len(batch), cp["mail.log"])
			f.saveCheckpoint(cp)
		}
	}
}

func (f *Forwarder) checkpoint() map[string]int64 {
	cp := map[string]int64{}
	if f.mailOff != nil {
		cp["mail.log"] = f.mailOff()
	}
	if f.rspamdOff != nil {
		cp["rspamd.log"] = f.rspamdOff()
	}
	return cp
}

func (f *Forwarder) send(ctx context.Context, batch []Record, cp map[string]int64) error {
	f.mu.Lock()
	conf := f.conf
	f.mu.Unlock()
	body, err := json.Marshal(ingestBatch{Version: version, Records: batch, Checkpoints: cp, Conf: conf})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url+"/ingest", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode == http.StatusUpgradeRequired {
		// Version lockstep failure — make it unmissable; retrying lets a rolling
		// upgrade self-heal once both sides match.
		return fmt.Errorf("VERSION MISMATCH refused by receiver — upgrade both to the same PLV version: %s", strings.TrimSpace(string(msg)))
	}
	return fmt.Errorf("receiver returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

func (f *Forwarder) saveCheckpoint(cp map[string]int64) {
	st := forwardState{Mail: cp["mail.log"], Rspamd: cp["rspamd.log"]}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := dataPath(f.dataDir, forwardStateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("forwarder: cannot persist checkpoint: %v", err)
		return
	}
	_ = os.Rename(tmp, dataPath(f.dataDir, forwardStateFile))
}

// --- enrolment (used by the wizard and the non-interactive `plv enroll`) ---

// enrollNode requests a client cert from the receiver using an HMAC ticket and writes
// ca.pem / cert.pem / key.pem into dataDir. The receiver is authenticated by its CA
// certificate (caPEM), copied out-of-band from the receiver — so the enrol connection is
// verified by a normal TLS chain + hostname check, with no trust-on-first-use. The
// private key is generated locally and never leaves the node.
func enrollNode(dataDir, receiverURL, cn, ticket string, caPEM []byte) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("invalid CA certificate (--ca)")
	}
	csrPEM, keyPEM, err := generateCSR(cn)
	if err != nil {
		return err
	}
	reqBody, _ := json.Marshal(enrollRequest{CN: cn, Ticket: ticket, CSR: string(csrPEM)})
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool, // verify the receiver's server cert against its CA (no InsecureSkipVerify)
			MinVersion: tls.VersionTLS12,
		}},
	}
	resp, err := client.Post(strings.TrimRight(receiverURL, "/")+"/enroll", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("enrol request failed (check the receiver URL, and that --ca is the receiver's CA): %w", err)
	}
	defer resp.Body.Close()
	var er enrollResponse
	_ = json.NewDecoder(resp.Body).Decode(&er)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("enrolment refused (%d): %s", resp.StatusCode, er.Error)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(dataPath(dataDir, certFile), []byte(er.Cert), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(dataPath(dataDir, keyFile), keyPEM, 0o600); err != nil {
		return err
	}
	// Persist the CA we verified against (the receiver echoes the same CA in the response).
	return os.WriteFile(dataPath(dataDir, caCertFile), caPEM, 0o644)
}
