# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.2] — 2026-05-31

### Added

- **Correlated queue-id switcher in the detail modal.** When one email fans out to several
  recipients (each delivered under its own queue id, sharing a Message-ID), the modal now shows a
  **Correlated** chip row — one chip per delivery, labelled with its recipient — and clicking a
  chip re-focuses the modal on that queue id's own sender, recipient, and outcome. Previously every
  correlated item rendered the same single recipient.
- **Recipient-verification rejects are correlated.** A `NOQUEUE` reject carries no Message-ID, so a
  front gateway's "user unknown" reject and the backend's reject of the same recipient-verification
  probe were never linked. PLV now pairs them via the backend response Postfix embeds verbatim in
  the gateway's detail (`… host <ip> said: <backend 550 line>`), scoped to the same recipient and a
  tight time window — so both rejects appear in the correlated switcher.

### Fixed

- **A message that defers before it delivers no longer stays stuck at "deferred".** Postfix
  retries a deferred message on several attempts and logs each one; the parser took the *first*
  status it saw, so a message that finally `status=sent` (delivered) was still shown as deferred.
  It now uses the terminal status, and the live-tail merge re-classifies a record when a later,
  more final status arrives (a content-filter/rspamd verdict such as spam/virus/blocked still
  overrides the raw status and is preserved).

## [1.0.1] — 2026-05-31

### Added

- **Distributed mode (mTLS forwarders → receiver)** — run PLV headless on each mail server as a
  *forwarder* that tails its logs and ships fully-merged records over mutually-authenticated TLS to
  a central *receiver* that presents one combined view. New env: `PLV_FORWARD_TO` (forwarder),
  `PLV_INGEST_ADDR` (receiver), `PLV_DATA_DIR` (PKI/state). A built-in CA and enrolment flow —
  `plv pki init` / `plv pki server` / `plv pki ticket`, `plv enroll`, and an interactive
  `plv wizard` — issue each forwarder a client certificate whose CN becomes its name in the new
  **Server** column. Records are keyed by `(server, queue-id)`; the same Message-ID seen on
  multiple servers is surfaced as a cross-node link.
- **Mail-direction classification** — every message is labelled **inbound / outbound / internal /
  relayed**, derived purely from log facts (public/private client & relay IPs, SASL auth,
  local-delivery markers, content-filter reinjection). `relayed` denotes true external→us→external
  transit. Surfaced as a sortable **Direction** column, filter chips, and a dashboard direction-card
  row.
- **Direction from Postfix config** — point `PLV_POSTFIX_CONF` at a read-only `/etc/postfix` and PLV
  derives **mynetworks** (trusted networks) and **local/hosted domains** (from `mydestination`,
  `relay_domains`, `virtual_*_domains`, following file-backed lookup tables) to sharpen direction —
  e.g. a content-filter gateway's own backend (a public IP in mynetworks) submitting outbound mail
  is no longer mistaken for inbound. Re-derived live when the config changes; in distributed mode
  each forwarder ships its own config so the receiver classifies per-server. Over-broad mynetworks
  entries (`>/16`) are dropped as a safety rail.
- **Servers page** — a header panel listing the Postfix settings PLV actually uses (mynetworks +
  local domains) per server, with a chip per forwarder in distributed mode.
- **Detail modal redesign** — the status is shown as an event cascade that mirrors the mail-path
  graphic (From → server hops → flagged result), with the recipient for delivered/sent or the
  verdict + reason for spam/blocked/etc. Adds a mail-path visualisation and a per-message log
  timeline (chronological, with a raw toggle).
- **Dashboard** — direction cards, a per-graph note of the active filters, a header summary of how
  many days of mail are loaded and the configured retention, and an adaptive Message-Volume axis
  (rolls hourly buckets up to days over long spans).
- **`AUTH_DISABLE`** — explicitly run with authentication off (otherwise a receiver auto-generates a
  login and prints it once on first start).
- **Build version chip in the header** — shows the running version, linking to the matching source on
  GitHub (the release page, the exact commit, or `main`) and a docs shortcut. A release build reads
  `v1.0.1`, an edge/commit build `v1.0.1+abc1234`, and a local build `v1.0.1-dev` — the link only
  resolves to a commit when one was stamped in, so it never points at a 404. (`appVersion` in
  `buildmeta.go` is the single source of truth for the semver.)
- **Signed releases** — the published multi-arch image is cosign-signed (keyless / Sigstore); each
  release also carries an SPDX SBOM, a Sigstore-signed `checksums.txt`, and SLSA build provenance
  (`multiple.intoto.jsonl`) over the image and the binaries. Verify the image with
  `cosign verify ghcr.io/ngn-au/plv:<version> --certificate-identity-regexp '^https://github.com/ngn-au/' --certificate-oidc-issuer https://token.actions.githubusercontent.com`.
- **Fuzz target** for the pure log parser (`go test -fuzz=FuzzParseLines`) — `parseLines` is the
  untrusted-input boundary, so it's fuzzed to stay total (never panics on a malformed line).

### Performance

- **The detail modal opens instantly.** Clicking a message now blurs the page and shows a loading
  state immediately, rather than waiting on the fetch; the content swaps in when ready (cached
  re-opens are instant). The `/api/detail` endpoint is also dramatically faster on large stores — it
  no longer rebuilds the global mail-direction index (a full-store rescan that IP-parses every
  record) on every open and once per related leg, computing a message's direction in a single
  Message-ID-scoped pass. On a 100k-record store a detail open dropped from seconds to ~1 ms.

### Changed

- **Supply-chain hardening** — the Docker base images are pinned by digest, and every GitHub Actions
  workflow declares a least-privilege top-level `permissions:` block (write scopes only on the jobs
  that need them).

- **rspamd `add header` / `rewrite subject` are now `delivered`, not `spam`** — those actions only
  *tag* a message that is still delivered; only `reject` blocks it. The spam score still rides along
  on the record. (The disposition model also distinguishes `delivered` from relayed `sent`, and adds
  `incomplete` for never-queued sessions.)
- Mail-history days + configured retention are surfaced in the header; the stat cards remain global
  totals while the charts/top-N reflect the active filters.

### Fixed

- Parser: extract the Subject for locally-submitted mail (`from local;`); capture the PMG `block`
  rule; `client=` hostname truncation; `received` now requires a queued `size=`; a NOQUEUE reject's
  server role reads `rejected`.
- rspamd log lines in the timeline are normalised to the Postfix offset so they sort chronologically
  instead of skewing by the host time zone.
- Mail path: a NOQUEUE reject stops at the server (nothing was relayed onward); inbound mail relayed
  to a backend shows the recipient instead of dead-ending.
- A filter that yields no rows now clears the volume chart instead of leaving a stale curve.
- **Graceful session expiry** — an expired session redirects to a "Session expired" login instead of
  surfacing a raw DataTables Ajax-error dialog.

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

[Unreleased]: https://github.com/ngn-au/plv/compare/v1.0.2...HEAD
[1.0.2]: https://github.com/ngn-au/plv/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/ngn-au/plv/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/ngn-au/plv/releases/tag/v1.0.0
