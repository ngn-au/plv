# Architecture

PLV is a single Go binary (no CGO) built around one rule: **parsing is pure, the store owns state.**
The web UI is embedded with `go:embed`, so the binary is self-contained.

## The pipeline

```
mail.log* / rspamd.log*  ──parse──▶  []Record  ──merge/dedup──▶  Store (in-memory)  ──serve──▶  HTTP/JSON ──▶ web UI
        (files)            (pure)                  (mutex)          (source of truth)      (handlers)
                                                       │
                                                       └──(optional)──▶ PostgreSQL (persist + reload)
```

1. **Startup parse.** `parseAllLogs` discovers `mail.log*` (oldest first, gzip-aware) and feeds each
   file's lines through the pure parser, adding records to the store. `parseAllRspamd` then correlates
   any `rspamd.log*` verdicts onto those records.
2. **Live tail.** A `Watcher` tails the active `mail.log` from the byte offset reached during the
   startup parse; an optional `RspamdWatcher` tails `rspamd.log`.
3. **Serve.** The HTTP server answers the UI's JSON requests straight from the in-memory store.
4. **Persist (optional).** With `DATABASE_URL` set, the store loads existing rows on boot and upserts
   every new/updated record.

## Files

| File | Responsibility |
|---|---|
| `main.go` | Flag parsing, startup wiring, signal handling, graceful shutdown; the `hash` and `version` subcommands; the retention purge loop. |
| `handlers.go` | The `http.Server`, route mux, the session store + auth middleware, and the four JSON API handlers. The web UI (`web/`) is embedded here via `go:embed`. |
| `parser.go` | **Pure** log-line parsing: the compiled regexes, queue-id grouping, `pmg-smtp-filter` verdict accumulation, NOQUEUE reject synthesis, and `deriveDisposition`. No I/O, no shared state. |
| `store.go` | The `Store`: the canonical `[]Record`, the `byQueueID` / `byDelivery` indexes, record merge/dedup, inbound↔outbound leg merging, search/sort/paginate, stats, and retention purge. All access behind one `sync.RWMutex`. |
| `watcher.go` | The `mail.log` live tail: rolling per-queue-id pending line groups, content-filter verdict accumulation, finalize on `": removed"`, and a stale flush after 5 minutes. |
| `rspamd.go` | `rspamd.log` parsing + live tail; correlates verdicts onto existing records by queue id (never creates rows); holds briefly-pending verdicts. |
| `db.go` | Optional PostgreSQL: connection pool, additive schema migration, load-all, batched upsert, and retention delete. |
| `web/` | `index.html` (the table + charts + detail view) and `login.html`, embedded in the binary. |

## The store is the source of truth

Everything the UI sees comes from the in-memory `Store`. It holds:

- `records []Record` — the canonical slice (append-only within a run, compacted only by retention
  purge).
- `byQueueID map[string]int` — resolves any queue id (inbound, outbound, or synthetic) to its record.
- `byDelivery map[string]int` — maps an onward (post-filter) queue id to the inbound primary that
  expects it, so the two legs merge into one item.
- `rspamdPending map[string]rspamdVerdict` — verdicts whose mail record hasn't been seen yet.

A `Record` keeps both the **raw** Postfix fields (`Status`, `StatusDetail`, …) and the **derived**
classification (`Disposition`, `Filter`, `SpamScore`, …). See [the disposition model](disposition.md)
for how the derived fields are computed and why the two legs of a scanned message are merged.

PostgreSQL, when enabled, is a durable mirror of the store, not a second source of truth: records are
loaded from it into memory on boot, and the store upserts back to it. Queries (search, stats) always
run against memory.

## Concurrency

- One `Store` mutex guards all record state. Read paths (`Search`, `Stats`, `GetDetail`,
  `GetStatus`) take the read lock; mutations take the write lock.
- The HTTP server, the `mail.log` watcher, the optional `rspamd.log` watcher, the retention ticker,
  and the session cleanup each run on their own goroutine, coordinated only through the store and a
  cancelable `context`.
- Database writes happen outside the store lock (the store snapshots the record under the lock, then
  releases it before the upsert) to keep parsing responsive.

## Startup and shutdown

`main` starts the HTTP server first (so the UI is reachable, showing a "parsing…" status), then runs
the blocking initial parse, marks the store ready, and starts the watchers. `SIGINT`/`SIGTERM`
cancels the context (stopping the watchers and retention loop) and shuts the server down.
