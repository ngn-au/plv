<!--
Thanks for contributing to PLV! Keep this PR small and focused.
The title must follow Conventional Commits, e.g. `fix: classify NOQUEUE 4xx as rejected`.
-->

## What & why

<!-- What does this change and why? This text becomes the CHANGELOG entry — write it for humans. -->

Closes #

## Type of change

- [ ] `fix` — bug fix (ships a regression test that fails before the fix)
- [ ] `feat` — new feature (ships docs)
- [ ] `docs` / `refactor` / `chore` / `test` / `perf` / `build` / `ci`

## Checklist

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `govulncheck ./...` passes
- [ ] Tests added/updated for the change, using **synthetic data only** (no real log lines)
- [ ] Docs updated under `docs/` and `CHANGELOG.md` has an `## [Unreleased]` entry

## Disposition / parsing impact

<!--
Required for changes to parser.go / store.go / watcher.go / rspamd.go / db.go.
One paragraph: what messages classify differently now, and what test proves it?
Write "n/a" if the change doesn't touch parsing or classification.
-->

## Confirmation

- [ ] This PR contains **no real mail-log data** — no real recipients, senders, subjects, or IPs.
