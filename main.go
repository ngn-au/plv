package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// version is the build version, injected at build time via
// -ldflags "-X main.version=vX.Y.Z". It defaults to "dev" for local builds.
// In distributed mode the forwarder and receiver must run the exact same version.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hash":
			runHashCommand()
			return
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		case "pki":
			runPKI(dataDir(), os.Args[2:])
			return
		case "enroll":
			runEnroll(os.Args[2:])
			return
		case "wizard":
			runWizard(dataDir())
			return
		}
	}

	addr := flag.String("addr", ":8080", "listen address for the web UI")
	logDir := flag.String("logdir", "/var/log", "directory containing mail.log files")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[plv] ")
	log.Printf("PLV %s starting", version)

	dd := dataDir()
	forwardTo := os.Getenv("PLV_FORWARD_TO")
	ingestAddr := os.Getenv("PLV_INGEST_ADDR")

	if forwardTo != "" && ingestAddr != "" {
		log.Fatalf("configure only one of PLV_FORWARD_TO (forwarder) or PLV_INGEST_ADDR (receiver), not both")
	}
	// Graceful fallback: distributed mode needs PKI material in the data dir. If it
	// isn't there (volume not mounted / never enrolled), warn and run standalone.
	if (forwardTo != "" || ingestAddr != "") && !pkiReady(dd) {
		log.Printf("WARNING: distributed mode requested but no PKI found in %s — run 'plv wizard' to set it up. Falling back to standalone.", dd)
		forwardTo, ingestAddr = "", ""
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Forwarder: headless, no DB, no web server. Parse and ship only. ──
	if forwardTo != "" {
		log.Printf("mode: forwarder → %s (data dir %s)", forwardTo, dd)
		fwd, err := NewForwarder(forwardTo, dd)
		if err != nil {
			log.Fatalf("forwarder: %v", err)
		}
		store := NewStore(nil)
		store.SetSink(fwd)
		st := loadForwardState(dd)
		mailOff, rspamdOff := bringUpParsing(ctx, store, *logDir, &st)
		fwd.SetCheckpointSources(mailOff, rspamdOff)
		// Ship this node's Postfix-derived direction facts to the receiver (per-origin),
		// so the receiver classifies this server's mail with its own mynetworks/domains.
		// Load synchronously BEFORE Run so the very first batch carries it, then watch.
		if confDir := os.Getenv("PLV_POSTFIX_CONF"); confDir != "" {
			if pc, err := loadPostfixConf(confDir); err == nil {
				fwd.SetConf(pc)
			}
			go watchPostfixConf(ctx, confDir, fwd.SetConf)
		}
		go fwd.Run(ctx)
		waitForSignal()
		cancel()
		return
	}

	// ── Standalone or Receiver: web UI, optional DB, optional ingest. ──
	receiver := ingestAddr != ""

	var db *DB
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		var err error
		db, err = OpenDB(dbURL)
		if err != nil {
			log.Fatalf("database: %v", err)
		}
		defer db.Close()
		if err := db.Migrate(); err != nil {
			log.Fatalf("database migration: %v", err)
		}
		log.Printf("database connected, persistence enabled")
	} else {
		log.Printf("database disabled (set DATABASE_URL to enable persistence)")
	}

	store := NewStore(db)
	if db != nil {
		if err := store.LoadFromDB(); err != nil {
			log.Fatalf("load from database: %v", err)
		}
	}

	auth := resolveAuth(receiver, dd)
	srv := NewServer(store, *addr, auth)
	go func() {
		log.Printf("listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	if receiver {
		is, err := NewIngestServer(ingestAddr, dd, store)
		if err != nil {
			log.Fatalf("ingest: %v", err)
		}
		log.Printf("mode: receiver (ingest %s, UI %s)", ingestAddr, *addr)
		go func() {
			if err := is.Run(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("ingest server: %v", err)
			}
		}()
	} else {
		log.Printf("mode: standalone (UI %s)", *addr)
	}

	bringUpParsing(ctx, store, *logDir, nil)

	// Direction-from-Postfix-config: derive mynetworks / local domains from a mounted
	// /etc/postfix dir (read-only) so a gateway's own backend submitting outbound mail
	// isn't mistaken for inbound. Re-derived live when the config changes.
	if confDir := os.Getenv("PLV_POSTFIX_CONF"); confDir != "" {
		go watchPostfixConf(ctx, confDir, store.SetPostfixConf)
	}

	if retStr := os.Getenv("RETENTION_DAYS"); retStr != "" {
		days, err := strconv.Atoi(retStr)
		if err != nil || days < 1 {
			log.Fatalf("RETENTION_DAYS must be a positive integer, got: %s", retStr)
		}
		retention := time.Duration(days) * 24 * time.Hour
		store.SetRetentionDays(days)
		log.Printf("retention policy: %d days", days)
		if purged := store.PurgeOlderThan(retention); purged > 0 {
			log.Printf("retention: purged %d expired records on startup", purged)
		}
		go func() {
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if n := store.PurgeOlderThan(retention); n > 0 {
						log.Printf("retention: purged %d expired records", n)
					}
				}
			}
		}()
	}

	waitForSignal()
	cancel()
	srv.Shutdown(context.Background())
}

// bringUpParsing parses existing logs and starts the live tailers. When resume is
// non-nil (a forwarder clean restart) and the checkpoint is still valid, historical
// re-parsing is skipped and tailing continues from the acked offsets; on a missing
// or stale checkpoint (or a rotated log) all history is re-parsed and the receiver
// dedupes by (origin, queue-id). Returns the mail and rspamd offset getters (for
// the forwarder checkpoint); either may be nil.
func bringUpParsing(ctx context.Context, store *Store, logDir string, resume *forwardState) (func() int64, func() int64) {
	mailLog := filepath.Join(logDir, "mail.log")
	rspamdLog := activeRspamdLog(logDir)

	var mailOffset, rspamdOffset int64
	resumed := false
	if resume != nil && resume.Mail > 0 {
		if info, err := os.Stat(mailLog); err == nil && info.Size() >= resume.Mail {
			mailOffset, rspamdOffset, resumed = resume.Mail, resume.Rspamd, true
			log.Printf("forwarder: resuming from checkpoint (mail offset %d, rspamd offset %d)", resume.Mail, resume.Rspamd)
		} else {
			log.Printf("forwarder: checkpoint stale or log rotated — re-parsing all logs (receiver dedupes)")
		}
	}

	if !resumed {
		mailOffset = parseAllLogs(logDir, store)
		parseAllRspamd(logDir, store)
		if rspamdLog != "" {
			if info, err := os.Stat(rspamdLog); err == nil {
				rspamdOffset = info.Size()
			}
		}
	}

	_, _, count := store.GetStatus()
	log.Printf("initial parse complete: %d records", count)
	store.SetReady()

	mw := NewWatcher(mailLog, store, mailOffset)
	go mw.Run(ctx)

	var rspamdOff func() int64
	if rspamdLog != "" {
		rw := NewRspamdWatcher(rspamdLog, store, rspamdOffset)
		go rw.Run(ctx)
		rspamdOff = rw.Offset
	}
	return mw.Offset, rspamdOff
}

// resolveAuth picks the auth config. Explicit AUTH_USERNAME/AUTH_PASSWORD_HASH
// always win. A receiver (network-exposed UI) otherwise auto-generates credentials
// unless AUTH_DISABLE is set; standalone keeps the historical "off unless set".
func resolveAuth(receiver bool, dataDir string) *AuthConfig {
	username := os.Getenv("AUTH_USERNAME")
	hash := os.Getenv("AUTH_PASSWORD_HASH")
	if username != "" && hash != "" {
		log.Printf("authentication enabled for user: %s", username)
		return &AuthConfig{Enabled: true, Username: username, PasswordHash: hash}
	}
	if isTruthy(os.Getenv("AUTH_DISABLE")) {
		log.Printf("authentication disabled (AUTH_DISABLE set)")
		return nil
	}
	if !receiver {
		log.Printf("authentication disabled (set AUTH_USERNAME and AUTH_PASSWORD_HASH to enable)")
		return nil
	}
	ac, err := loadOrCreateAuth(dataDir)
	if err != nil {
		log.Printf("WARNING: could not set up auto authentication (%v); the UI is UNAUTHENTICATED", err)
		return nil
	}
	return ac
}

type authFileData struct {
	Username string `json:"username"`
	Hash     string `json:"password_hash"`
}

// loadOrCreateAuth loads receiver credentials from the data dir, generating them
// (and printing the password once) on first run so the bcrypt hash stays stable.
func loadOrCreateAuth(dataDir string) (*AuthConfig, error) {
	path := dataPath(dataDir, "auth.json")
	if b, err := os.ReadFile(path); err == nil {
		var d authFileData
		if json.Unmarshal(b, &d) == nil && d.Username != "" && d.Hash != "" {
			log.Printf("authentication enabled for user: %s (from %s)", d.Username, path)
			return &AuthConfig{Enabled: true, Username: d.Username, PasswordHash: d.Hash}, nil
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil { // holds the generated credential file
		return nil, err
	}
	pw, err := randomPassword()
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	d := authFileData{Username: "admin", Hash: string(hash)}
	b, _ := json.MarshalIndent(d, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	log.Printf("┌─ receiver auth auto-generated (override with AUTH_USERNAME/AUTH_PASSWORD_HASH, or AUTH_DISABLE to turn off)")
	log.Printf("│  username: admin")
	log.Printf("│  password: %s", pw)
	log.Printf("└─ hash stored in %s (shown once)", path)
	return &AuthConfig{Enabled: true, Username: "admin", PasswordHash: string(hash)}, nil
}

func randomPassword() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func dataDir() string {
	if d := os.Getenv("PLV_DATA_DIR"); d != "" {
		return d
	}
	return "/data"
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// watchPostfixConf derives the direction facts (mynetworks / local domains) from a
// Postfix config dir and re-derives them on a poll, applying the new set to the store
// only when it actually changes. Best-effort: a missing/unreadable dir just leaves the
// store on the IP heuristic.
func watchPostfixConf(ctx context.Context, dir string, set func(*PostfixConf)) {
	last := ""
	apply := func() {
		pc, err := loadPostfixConf(dir)
		if err != nil {
			if last == "" {
				log.Printf("postfix config %s: %v (direction uses the IP heuristic only)", dir, err)
			}
			return
		}
		sig := pc.signature()
		if sig == last {
			return
		}
		set(pc)
		if last == "" {
			log.Printf("postfix config loaded from %s: %d local domain(s), %d trusted network(s)", dir, len(pc.LocalDomains), len(pc.Networks))
		} else {
			log.Printf("postfix config changed: %d local domain(s), %d trusted network(s)", len(pc.LocalDomains), len(pc.Networks))
		}
		last = sig
	}
	apply()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			apply()
		}
	}
}

func waitForSignal() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
}

// runEnroll is the non-interactive enrolment path: plv enroll --to URL --cn NAME --ticket T.
func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	to := fs.String("to", "", "receiver URL, e.g. https://host:8443")
	cn := fs.String("cn", "", "this node's name (client-cert CN)")
	ticket := fs.String("ticket", "", "enrolment ticket from 'plv pki ticket'")
	caPath := fs.String("ca", "", "path to the receiver's CA certificate (ca.pem), copied out-of-band")
	_ = fs.Parse(args)
	if *to == "" || *cn == "" || *ticket == "" || *caPath == "" {
		fmt.Fprintln(os.Stderr, "usage: plv enroll --to https://host:port --cn <name> --ticket <ticket> --ca <ca.pem>")
		os.Exit(1)
	}
	caPEM, err := os.ReadFile(*caPath)
	if err != nil {
		fatalErr(fmt.Errorf("read CA file %q: %w", *caPath, err))
	}
	if err := enrollNode(dataDir(), *to, *cn, *ticket, caPEM); err != nil {
		fatalErr(err)
	}
	fmt.Printf("enrolled %q against %s\n", *cn, *to)
	fmt.Printf("certs written to %s\n", dataDir())
	fmt.Printf("run the forwarder:  PLV_FORWARD_TO=%s PLV_DATA_DIR=%s plv -logdir /var/log\n", *to, dataDir())
}

func runHashCommand() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s hash <password>\n", os.Args[0])
		os.Exit(1)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(os.Args[2]), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(hash))
}
