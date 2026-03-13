package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash" {
		runHashCommand()
		return
	}

	addr := flag.String("addr", ":8080", "listen address")
	logDir := flag.String("logdir", "/var/log", "directory containing mail.log files")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[plv] ")

	var auth *AuthConfig
	username := os.Getenv("AUTH_USERNAME")
	passwordHash := os.Getenv("AUTH_PASSWORD_HASH")
	if username != "" && passwordHash != "" {
		auth = &AuthConfig{
			Enabled:      true,
			Username:     username,
			PasswordHash: passwordHash,
		}
		log.Printf("authentication enabled for user: %s", username)
	} else {
		log.Printf("authentication disabled (set AUTH_USERNAME and AUTH_PASSWORD_HASH to enable)")
	}

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
	srv := NewServer(store, *addr, auth)

	go func() {
		log.Printf("listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	offset := parseAllLogs(*logDir, store)
	_, _, count := store.GetStatus()
	log.Printf("initial parse complete: %d records", count)
	store.SetReady()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mailLog := filepath.Join(*logDir, "mail.log")
	watcher := NewWatcher(mailLog, store, offset)
	go watcher.Run(ctx)

	if retStr := os.Getenv("RETENTION_DAYS"); retStr != "" {
		days, err := strconv.Atoi(retStr)
		if err != nil || days < 1 {
			log.Fatalf("RETENTION_DAYS must be a positive integer, got: %s", retStr)
		}
		retention := time.Duration(days) * 24 * time.Hour
		log.Printf("retention policy: %d days", days)

		purged := store.PurgeOlderThan(retention)
		if purged > 0 {
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

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down...")
	cancel()
	srv.Shutdown(context.Background())
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
