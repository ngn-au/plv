# Contributing to PLV

> **Why this document exists.** PLV parses mail logs — real ones contain recipients, senders,
> subjects, and client IPs. A careless change can leak that data, mis-classify a message's
> disposition (so quarantined spam shows as delivered), or break the live tail. The rules below are
> short on purpose — read them before opening a PR so review is about the idea, not the conventions.

---

## The one rule that matters most

**Never commit log data.** Not a real `mail.log`, not a "lightly anonymized" one, not a paste in an
issue or PR. The `logs/` directory is git-ignored and exists only as a local scratch area. Every
test and example in this repo uses **synthetic data**: `example.com` / `.net` / `.org` addresses and
RFC 5737 documentation IPs (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`). New parser tests
must do the same. If you can't reproduce a bug without real data, describe the log *shape* and craft
a synthetic line that matches it.

---

## Ground rules

1. **Every change is reviewed.** No direct pushes to `main`. PR + at least one approval. The parsing
   / correlation logic (`parser.go`, `store.go`, `rspamd.go`, `watcher.go`) and the auth/session
   code (`handlers.go`) warrant an extra-careful read.
2. **Every PR ships its own tests and docs.** A bug fix ships a regression test that fails before the
   fix. A behavior change updates the relevant page under [`docs/`](./docs).
3. **Trunk-based.** Short-lived branches, squash-merge to `main`, no long-running forks.
4. **Conventional Commits** for PR titles: `feat:`, `fix:`, `docs:`, `refactor:`, `chore:`, `test:`,
   `perf:`, `build:`, `ci:`. The PR body is the changelog entry — write it for humans and add a line
   to [`CHANGELOG.md`](./CHANGELOG.md) under `## [Unreleased]`.
5. **CI must be green.** `gofmt`, `go vet`, `go test -race ./...`, and `govulncheck ./...` all pass
   before merge. Run them locally first (see [Local checks](#local-checks)).

---

## Code standards (Go)

- **`gofmt` is law.** Run `gofmt -w .` (or `go fmt ./...`) before committing. CI fails on any
  unformatted file (`gofmt -l .` must print nothing).
- **`go vet` is clean.** No vet findings land on `main`.
- **Errors are wrapped, not swallowed.** Return `fmt.Errorf("context: %w", err)` so the cause
  survives. The startup path uses `log.Fatalf` deliberately; request handlers return an HTTP error.
- **No abbreviations** in identifiers beyond well-understood ones (`id`, `tls`, `ip`, `qid`, `db`,
  `url`). `disposition`, not `disp`, in exported/long-lived names.
- **Name the magic.** A bare `5*time.Minute` in a loop deserves a comment or a named constant when
  the value encodes a policy (e.g. the stale-flush window).
- **Keep the standard library first.** PLV deliberately has only two dependencies
  (`github.com/lib/pq`, `golang.org/x/crypto`). Reach for the stdlib before adding a third.

### Boundaries (the hard rules)

- **Parsing is pure; the store owns state.** Functions in `parser.go` turn `[]string` of log lines
  into `[]Record` with no I/O and no shared state — that's why they're trivially testable. All
  mutation, merging, and dedup lives in `store.go` behind the `Store` mutex. Don't reach into the
  store from a parser function, and don't put regex parsing in the store.
- **One regex per concept, compiled once.** Log-line patterns are package-level `regexp.MustCompile`
  vars at the top of `parser.go` / `rspamd.go`. Add new patterns there with a comment naming what
  they match; don't compile a regex inside a hot loop.
- **Correlation never invents rows.** rspamd verdicts only *enrich* an existing mail record matched
  by queue id; they never create a standalone record. The only synthesized records are NOQUEUE
  rejections (which Postfix genuinely never assigns a queue id).
- **The effective `Disposition` is derived, the raw `Status` is sacred.** Keep the original Postfix
  `Status` / `StatusDetail` untouched on the record; classification writes only `Disposition` (and
  the filter fields). The UI shows disposition as the badge and the raw status in the tooltip.
- **The DB schema is migrated additively.** New columns are added with `ALTER TABLE … ADD COLUMN IF
  NOT EXISTS` in `db.go`'s `Migrate()`, with matching `LoadAll` / `UpsertRecords` columns. Existing
  installs must upgrade in place without a manual migration step.

### Project layout

```
main.go        Flag parsing, startup wiring, signal handling, the `hash` / `version` subcommands.
handlers.go    HTTP server, routes, session store, auth middleware, the JSON API handlers.
parser.go      Pure log-line parsing: regexes, queue-id grouping, PMG filter correlation,
               NOQUEUE rejections, disposition derivation. No I/O, no shared state.
store.go       In-memory Store (the source of truth): record merge/dedup, inbound↔outbound leg
               merging, search/sort/paginate, stats, retention purge.
watcher.go     Live tail of mail.log: rolling pending groups, verdict accumulation, finalize.
rspamd.go      rspamd.log parsing + live tail; correlates verdicts onto existing records by qid.
db.go          Optional PostgreSQL persistence: schema migration, load-all, batched upsert, purge.
web/           Embedded UI (index.html, login.html) served via go:embed.
docs/          User and developer documentation.
```

---

## Testing

- **Unit tests live next to the code** in `_test.go` files in the same package. Prefer pure,
  hermetic tests with no network or filesystem — the parsing pipeline is built to be tested this way
  (feed `parseLines` a `[]string`, assert on the `[]Record`).
- **The things that must stay covered:** PMG spam/virus quarantine classification, inbound↔outbound
  leg merging into one item, NOQUEUE reject classification, rspamd verdict correlation, and
  `deriveDisposition` mapping. See [`parser_test.go`](./parser_test.go).
- **Every bug fix ships a test that fails before the fix.** No exceptions.
- **Synthetic data only** (see [the one rule](#the-one-rule-that-matters-most)).

### Local checks

The project builds with the Go toolchain pinned in [`go.mod`](./go.mod) (currently 1.26). With a
matching local toolchain:

```sh
gofmt -l .                     # must print nothing
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

No local Go? Run the checks in a container:

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c '
  apk add --no-cache git >/dev/null
  test -z "$(gofmt -l .)" && go vet ./... && go test ./...'
```

---

## Documentation standards

Docs rot when they aren't part of the change. Keep them in the same PR as the code.

- **Comment the _why_, not the _what_.** `// PMG re-injects under a new queue id after accept` is
  useful; `// loop over records` is not. The parser is dense with regexes — a one-line comment
  naming what each pattern matches earns its keep.
- **User-facing docs go in [`docs/`](./docs).** If you add an environment variable, a flag, or a new
  disposition, update [`docs/configuration.md`](./docs/configuration.md) /
  [`docs/disposition.md`](./docs/disposition.md) in the same PR.

---

## Security practices

- **Authorized testing only.** If you find a vulnerability, follow [`SECURITY.md`](./SECURITY.md);
  do not open a public issue.
- **No secrets, and no log data, in git, issues, or PRs.** Especially `AUTH_PASSWORD_HASH`,
  `DATABASE_URL`, and anything from a real `mail.log`.
- **Parse defensively.** Log lines are untrusted input. New regexes must be anchored/bounded enough
  not to backtrack pathologically on a hostile line, and parsing must never panic on malformed input
  (return zero values, skip the line).

---

## Dependencies

- **The module graph is the source of truth.** `go.mod` / `go.sum` are committed; CI and the Docker
  build use them as-is. Run `go mod tidy` after a dependency change.
- **Justify every new dependency** in the PR: what does it buy that a little stdlib code wouldn't?
  PLV's two-dependency footprint is a feature.

---

## Governance, for now

The project is small: one maintainer with final say. Disagreement is welcome **in writing**, not in
revert wars. As the project grows this will become a written governance model.

PLV is licensed under the **MIT License**. By contributing you agree your contribution is licensed
under the same terms.
