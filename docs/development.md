# Development

PLV is a single Go module with no code generation and no frontend build step (the UI is hand-written
HTML embedded via `go:embed`). If you have Go installed, you can build and test it immediately.

## Prerequisites

- **Go 1.26+** (the version is pinned in [`go.mod`](../go.mod); CI uses it via `go-version-file`).
- *(optional)* **Docker** — to build the image or run the full Compose stack.
- *(optional)* **[act](https://github.com/nektos/act)** — to run the CI jobs locally in the runner
  image.
- *(optional)* **PostgreSQL** — only if you're working on the persistence path; the test suite does
  not need it.

## Build & run

```bash
git clone https://github.com/ngn-au/plv.git
cd plv

make build                         # → ./plv  (or: go build -o plv .)
make run LOGDIR=/path/to/logs      # → http://localhost:8080
#   equivalently: go run . -addr :8080 -logdir /path/to/logs
```

There is **no committed sample log**. To exercise the parser end to end, point `-logdir` at a
directory holding a `mail.log` you control, or write a synthetic one. **Never** use real production
logs as a fixture, and never commit one (`logs/` is git-ignored for exactly this reason).

## The local gate

`make check` runs the same checks CI gates on, directly on the host:

```bash
make check        # = fmt-check + vet + test (race) + vuln
```

Individually:

| Command | What |
|---|---|
| `make fmt` | `gofmt -w .` — format everything. |
| `make fmt-check` | Fail if any file isn't gofmt-clean (what CI checks). |
| `make vet` | `go vet ./...`. |
| `make test` | `go test -race ./...`. |
| `make vuln` | `govulncheck ./...` (downloaded on demand). |
| `make build` | Build `./plv` (pass `VERSION=v1.2.3` to stamp the version). |
| `make docker-build` | Build the production image locally. |

Run `make help` for the full list.

## Running CI locally with act

The CI `go` and `govulncheck` jobs run unmodified inside the GitHub Actions runner image via
[act](https://github.com/nektos/act). The committed [`.actrc`](../.actrc) pins the runner image and
defaults to the CI workflow, so no flags are needed:

```bash
make ci-local       # runs `act -j go` then `act -j govulncheck`

# or invoke a single job directly:
act -j go
act -j govulncheck
```

`act` uses your host-native architecture (don't force `linux/amd64` on Apple Silicon — the Go
toolchain under QEMU is slow and flaky). The **docker** build job, **CodeQL**, **Scorecard**, and
**dependency-review** need a real GitHub-hosted runner and are not run under act — use `make
docker-build` to validate the image locally instead.

## Writing tests

Tests live next to the code in `_test.go` files in `package main`. The parser is built to be tested
without any I/O: feed `parseLines` a `[]string` of log lines and assert on the returned `[]Record`.
See [`parser_test.go`](../parser_test.go) for the patterns.

**Use synthetic data only** — `example.com` / `.net` / `.org` addresses and RFC 5737 documentation
IPs (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`). Every bug fix ships a test that fails
before the fix.

## How to add a parser case or disposition

The classification model is documented in [disposition.md](disposition.md). To add or change an
outcome, touch these together:

1. **The pattern** — a new `regexp.MustCompile` var at the top of `parser.go` or `rspamd.go`, with a
   one-line comment naming what it matches.
2. **The mapping** — `deriveDisposition` (Postfix path) and/or `rspamdDisposition` (rspamd path).
3. **The stats** — the bucketing `switch` in `Store.Stats`, so the new outcome is counted.
4. **Persistence** — if you add a `Record` field that must survive a restart, add a column in
   `db.go`'s `Migrate()` (`ALTER TABLE … ADD COLUMN IF NOT EXISTS`) and wire it into `LoadAll` and
   the upsert.
5. **Docs + tests** — update [disposition.md](disposition.md) and the README table, and add a test
   with synthetic data.

## Project layout

See [architecture.md](architecture.md) for the file-by-file breakdown and the parse → store → serve
pipeline.

## Releasing

Releases are tag-driven. Maintainers:

1. Move the `## [Unreleased]` notes in [`CHANGELOG.md`](../CHANGELOG.md) under a new `## [X.Y.Z]`
   heading with the date, and update the compare links.
2. Push a `vX.Y.Z` tag. That triggers:
   - **release.yml** — cross-compiled binaries (linux/macOS × amd64/arm64) + `SHA256SUMS` attached to
     the GitHub release.
   - **docker-publish.yml** — a multi-arch image pushed to `ghcr.io/ngn-au/plv` tagged `X.Y.Z`, `X.Y`,
     and `latest`.

`main` pushes publish the `edge` image tag.
