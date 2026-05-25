# CLAUDE.md

Guidance for AI coding agents working in this repository. This file is read before any task.

## The hard constraint, first

**Never read, quote, copy, or commit anything from the `logs/` directory.** It is git-ignored and
holds the operator's local *example* mail logs — real recipients, senders, subjects, and IPs. It is
not a fixture source and not a reference. All examples and tests in this repo use **synthetic data**:
`example.com` / `.net` / `.org` addresses and RFC 5737 documentation IPs (`192.0.2.0/24`,
`198.51.100.0/24`, `203.0.113.0/24`). If you need a sample log line, write a synthetic one — never
surface anything from `logs/`.

## What this repository is

**PLV — Postfix Log Viewer** — a lightweight, self-hosted web UI for searching and visualising
Postfix mail logs. It parses every `mail.log*` file (including gzipped rotations) on startup, then
live-tails the active `mail.log`. Its defining feature is **content-filter awareness**: when Postfix
hands a message to a local scanner (Proxmox Mail Gateway's `pmg-smtp-filter`, or rspamd) it logs only
`status=sent`, which is misleading if the scanner then quarantines or blocks the mail. PLV correlates
the scanner's verdict back to the message and shows the real **effective disposition**.

The stack is a single Go binary (Go 1.26, no CGO), standard-library HTTP server, with the web UI
embedded via `go:embed`. Two dependencies only: `github.com/lib/pq` (optional PostgreSQL) and
`golang.org/x/crypto` (bcrypt). Licensed under **MIT**.

## Start here

1. **[`README.md`](./README.md)** — public-facing overview, quickstart, environment variables.
2. **[`docs/disposition.md`](./docs/disposition.md)** — the correlation/classification model. This is
   the conceptual heart of PLV; read it before touching `parser.go` / `store.go` / `rspamd.go`.
3. **[`docs/architecture.md`](./docs/architecture.md)** — how the files fit together.
4. **[`CONTRIBUTING.md`](./CONTRIBUTING.md)** — code standards, boundaries, testing, the no-log-data
   rule. Following these saves review churn.

## How to run things

```sh
# Local run against a directory of mail.log files (read-only is the production posture):
go run . -addr :8080 -logdir /path/to/logs       # http://localhost:8080

# Generate a bcrypt hash for the login password:
go run . hash 'my-secret-password'

# Print the build version:
go run . version

# Containerized (the published image is ghcr.io/ngn-au/plv):
docker compose up -d
```

Authentication is **off** unless both `AUTH_USERNAME` and `AUTH_PASSWORD_HASH` are set. Persistence is
**off** unless `DATABASE_URL` is set (otherwise everything is in memory). Retention purging is **off**
unless `RETENTION_DAYS` is set.

## The checks that gate a change

```sh
gofmt -l .            # must print nothing
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

CI runs all four plus a Docker build. Make them green locally before proposing a change.

## Project layout

```
main.go        Flag parsing, startup wiring, signals, `hash` / `version` subcommands.
handlers.go    HTTP server, routes, session store, auth middleware, JSON API handlers.
parser.go      PURE log-line parsing: regexes, queue-id grouping, PMG filter correlation,
               NOQUEUE rejections, disposition derivation. No I/O, no shared state — fully testable.
store.go       In-memory Store (source of truth): merge/dedup, inbound↔outbound leg merging,
               search/sort/paginate, stats, retention purge. All state behind one RWMutex.
watcher.go     Live tail of mail.log: rolling pending groups + verdict accumulation, finalize on
               ": removed", stale flush after 5 minutes.
rspamd.go      rspamd.log parse + live tail; correlates verdicts onto existing records by qid only.
db.go          Optional PostgreSQL: additive schema migration, load-all, batched upsert, purge.
web/           index.html + login.html, embedded via go:embed.
docs/          User + developer docs.
```

## Working conventions

### The parse → store split

`parser.go` is pure: lines in, records out, no side effects. Anything that mutates, dedups, merges,
or persists belongs in `store.go` (behind the mutex) or `db.go`. This separation is what makes the
parser testable with a `[]string` literal. Don't blur it.

### Disposition is derived; status is sacred

A record keeps the raw Postfix `Status`/`StatusDetail` untouched. Classification writes only
`Disposition` (and the `Filter*` / `SpamScore` fields). The UI shows `dispositionOrStatus()` as the
badge and the raw status in the tooltip. When adding a new outcome, update `deriveDisposition`,
`rspamdDisposition`, the stats bucketing in `Store.Stats`, and `docs/disposition.md` together.

### One item per message

A scanned-and-accepted message has two queue ids (inbound scanner leg + re-injected outbound leg).
They are linked via the `accept mail … (<qid>)` line and merged into one row (`mergeDeliveryLeg`),
with the outbound leg marked `Subsumed` so it isn't listed separately. Either id resolves to the
merged item. Preserve this when changing merge logic — regressions here double-count mail.

### Schema migrations are additive and idempotent

New DB columns go in `Migrate()` as `ALTER TABLE … ADD COLUMN IF NOT EXISTS`, with the column added
to both `LoadAll` and the upsert in `db.go`. Existing installs must upgrade in place.

### Comments

Explain **why** something is done a non-obvious way, not what the code does. The regex blocks are
the exception: a one-line comment naming what each pattern matches is expected, because the patterns
are the spec.

## When working in this repo

- **Read [`docs/disposition.md`](./docs/disposition.md) before touching correlation/classification.**
  It encodes subtle ordering assumptions (filter lines arrive before the Postfix hand-off; rspamd
  verdicts can arrive just before the record is finalized).
- **Keep the dependency footprint at two.** Prefer the standard library.
- **Don't add a top-level doc casually.** New material is a section in an existing `docs/` page unless
  it's genuinely a new topic.
- **Test with synthetic data, always.** And never echo `logs/` content into the conversation or the
  tree.
