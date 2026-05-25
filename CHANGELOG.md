# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] — 2026-05-26

First public release.

### Added

- **Postfix log viewer** — parses every `mail.log*` file in the log directory on startup
  (including gzipped rotations, oldest first), then live-tails the active `mail.log` for new
  entries. Each message is grouped by Postfix queue id into a single record.
- **Effective disposition, not raw status** — a content-filter-aware classification layer maps
  each message to an effective outcome (`sent` / `spam` / `blocked` / `virus` / `rejected` /
  `bounced` / `deferred` / `received`). The raw Postfix `status` and the scanner verdict are
  preserved in the row tooltip and the detail view.
- **Proxmox Mail Gateway (`pmg-smtp-filter`) correlation** — when Postfix hands a message to the
  local content filter it logs only `status=sent`, which is misleading if the filter then
  quarantines or blocks the mail. PLV matches the filter session id carried in the hand-off line
  back to the filter's own log lines (SA score, quarantine / accept / block) to derive the real
  disposition, spam score, and rule.
- **One item per scanned message** — a scanned-and-accepted message has two Postfix queue ids
  (the inbound leg to the scanner and the re-injected outbound leg); PLV links them via the
  `accept mail … (<queue-id>)` line and merges them into one row that keeps the inbound metadata
  and shows the real destination relay and final delivery status. Either queue id resolves to the
  merged item in search and detail.
- **rspamd correlation** — when an rspamd host logs verdicts to its own `rspamd.log*`, PLV joins
  each task-summary line to the matching mail record by queue id (it never creates standalone rows
  from rspamd lines), enriching the record with the rspamd action and score.
- **NOQUEUE rejections** — SMTP-time rejections (RBL, relay, postscreen, policy) that never get a
  queue id are captured as standalone records with a deterministic synthetic id, and are always
  classified as `rejected`.
- **Search & filtering** — a server-paginated, sortable table (DataTables protocol) with global
  full-text search and per-field filters (from, to, client, relay, status, queue id, TLS).
- **Statistics** — totals by disposition, top senders/recipients, and an hourly volume series.
- **Optional PostgreSQL persistence** — set `DATABASE_URL` to persist records across restarts; PLV
  creates the schema and indexes on boot, loads stored records into memory, and upserts every new
  or updated record. Without it, PLV runs purely in memory.
- **Data retention** — set `RETENTION_DAYS` to purge records older than N days on startup and every
  hour, from both memory and the database.
- **Authentication** — optional username + bcrypt-hashed password login with HTTP-only session
  cookies; a built-in `plv hash <password>` helper generates the hash. Disabled when unset.
- **Single static binary** — Go, no CGO; the web UI is embedded with `go:embed`. Distributed as a
  multi-arch (amd64/arm64) container image on **GHCR** and as standalone release binaries for
  linux/macOS.
- **`plv version`** — prints the build version injected at build time via `-ldflags`.

### Engineering baseline

- Unit-test suite over the correctness-critical parsing/correlation logic (PMG quarantine, leg
  merging, NOQUEUE classification, rspamd join, disposition derivation) using only synthetic
  fixtures (RFC 5737 documentation IPs and `example.com` addresses).
- CI enforces `gofmt`, `go vet`, `go test -race`, and `govulncheck`, plus a Docker build.
- CodeQL (SAST), OpenSSF Scorecard, and dependency review run in CI; Dependabot keeps Go modules,
  GitHub Actions, and the base image up to date.

[Unreleased]: https://github.com/ngn-au/plv/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/ngn-au/plv/releases/tag/v1.0.0
