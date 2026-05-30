package main

// Receiver side of the distributed mode: an mTLS HTTP listener that (a) enrols
// forwarders (signs their CSR against an HMAC ticket) and (b) accepts batches of
// already-parsed records, stamping origin from the verified client-cert CN.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

// ingestBatch is the forwarder->receiver payload. Records are full snapshots
// (already merged/correlated). Checkpoints are the forwarder's per-logfile byte
// offsets; the receiver echoes them back on success so the forwarder can advance.
type ingestBatch struct {
	Version     string           `json:"version"`
	Records     []Record         `json:"records"`
	Checkpoints map[string]int64 `json:"checkpoints,omitempty"`
	Conf        *ConfPayload     `json:"conf,omitempty"` // this forwarder's Postfix-derived direction facts
}

type ingestAck struct {
	Accepted    int              `json:"accepted"`
	Checkpoints map[string]int64 `json:"checkpoints,omitempty"`
	Error       string           `json:"error,omitempty"`
}

type enrollRequest struct {
	CN     string `json:"cn"`
	Ticket string `json:"ticket"`
	CSR    string `json:"csr"`
}

type enrollResponse struct {
	Cert  string `json:"cert"`
	CA    string `json:"ca"`
	Error string `json:"error,omitempty"`
}

const maxIngestBody = 64 << 20 // 64 MiB per batch

// IngestServer is the receiver's mTLS listener.
type IngestServer struct {
	addr    string
	dataDir string
	store   *Store
	srv     *http.Server
}

func NewIngestServer(addr, dataDir string, store *Store) (*IngestServer, error) {
	tlsCfg, err := serverTLSConfig(dataDir)
	if err != nil {
		return nil, err
	}
	is := &IngestServer{addr: addr, dataDir: dataDir, store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("/enroll", is.handleEnroll)
	mux.HandleFunc("/ingest", is.handleIngest)
	is.srv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		TLSConfig:    tlsCfg,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	return is, nil
}

func (is *IngestServer) Run() error {
	ln, err := net.Listen("tcp", is.addr)
	if err != nil {
		return err
	}
	log.Printf("ingest: mTLS listener on %s", is.addr)
	// Certs come from TLSConfig.Certificates, so the file args are empty.
	return is.srv.ServeTLS(ln, "", "")
}

func (is *IngestServer) Shutdown(ctx context.Context) { _ = is.srv.Shutdown(ctx) }

// handleEnroll signs a forwarder's CSR if its HMAC ticket is valid. No client
// cert is required here (the node doesn't have one yet).
func (is *IngestServer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, enrollResponse{Error: "bad request"})
		return
	}
	salt, err := loadTicketSalt(is.dataDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, enrollResponse{Error: "receiver PKI not initialised"})
		return
	}
	if req.CN == "" || !validTicket(salt, req.CN, req.Ticket) {
		log.Printf("ingest: enrol rejected for CN %q (invalid ticket)", logSafe(req.CN))
		writeJSON(w, http.StatusForbidden, enrollResponse{Error: "invalid ticket"})
		return
	}
	certPEM, err := signCSR(is.dataDir, []byte(req.CSR), req.CN)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, enrollResponse{Error: err.Error()})
		return
	}
	caPEM, err := os.ReadFile(dataPath(is.dataDir, caCertFile))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, enrollResponse{Error: "cannot read CA"})
		return
	}
	log.Printf("ingest: enrolled forwarder %q", logSafe(req.CN))
	writeJSON(w, http.StatusOK, enrollResponse{Cert: string(certPEM), CA: string(caPEM)})
}

// handleIngest accepts a batch of records from a verified forwarder.
func (is *IngestServer) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		writeJSON(w, http.StatusUnauthorized, ingestAck{Error: "client certificate required"})
		return
	}
	cn := r.TLS.PeerCertificates[0].Subject.CommonName
	if cn == "" {
		writeJSON(w, http.StatusForbidden, ingestAck{Error: "client certificate has no common name"})
		return
	}

	var batch ingestBatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBody)).Decode(&batch); err != nil {
		writeJSON(w, http.StatusBadRequest, ingestAck{Error: "bad request body"})
		return
	}
	// Version lockstep: forwarder and receiver must be byte-identical builds.
	if batch.Version != version {
		log.Printf("ingest: refused %q — version mismatch (forwarder %q, receiver %q)", logSafe(cn), logSafe(batch.Version), version)
		writeJSON(w, http.StatusUpgradeRequired, ingestAck{
			Error: fmt.Sprintf("version mismatch: receiver is %s, forwarder is %s — both must run the exact same PLV version", version, logSafe(batch.Version)),
		})
		return
	}

	if batch.Conf != nil && is.store.SetPostfixConfForOrigin(cn, batch.Conf.toConf()) {
		log.Printf("ingest: %s postfix config (%d domain(s), %d network(s))", logSafe(cn), len(batch.Conf.LocalDomains), len(batch.Conf.Networks))
	}
	n := is.store.IngestSnapshots(cn, batch.Records)
	if len(batch.Records) > 0 { // skip the heartbeat (config-only, empty) batches
		log.Printf("ingest: %s batch=%d accepted=%d total=%d", logSafe(cn), len(batch.Records), n, is.store.Count())
	}
	writeJSON(w, http.StatusOK, ingestAck{Accepted: n, Checkpoints: batch.Checkpoints})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
