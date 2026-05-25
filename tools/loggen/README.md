# loggen — synthetic PMG sample-log generator

`loggen` generates realistic **Proxmox Mail Gateway (PMG)** style Postfix logs so you can run PLV
against demo data — for screenshots, local testing, or a UI walkthrough — without ever touching real
mail logs.

> **Everything it emits is fictional.** Addresses use the reserved documentation domains
> (`example.com` / `.net` / `.org` and the `.example` TLD, RFC 2606); IPs come from the RFC 5737
> documentation ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`). It **writes only** to
> the output directory and **reads no existing logs**.

## Quick start

```bash
# From the repo root: 30 days of logs into ./logs/sample (git-ignored)
go run ./tools/loggen

# Then point PLV at it:
go run . -logdir logs/sample
#   → http://localhost:8080
```

That's the whole loop. `logs/sample/` is git-ignored, so the generated data never gets committed.

## Flags

| Flag | Default | Description |
|---|---|---|
| `-days` | `30` | Days of history to generate, ending today. |
| `-per-day` | `800` | Approximate messages per **weekday** (weekends are ~45% lighter, with a day/night curve). |
| `-out` | `logs/sample` | Output directory (created if missing; never read). |
| `-seed` | `1` | PRNG seed — same seed + flags ⇒ identical output (reproducible screenshots). |
| `-tz` | `UTC` | Timezone for log timestamps, e.g. `UTC` or `Australia/Sydney`. |

```bash
go run ./tools/loggen -days 60 -per-day 2000 -seed 7          # heavier, longer history
go run ./tools/loggen -tz Australia/Sydney -out /tmp/plvdata  # local-time stamps, custom dir
```

On completion it prints a summary: total messages, the per-disposition breakdown, and the per-file
line counts and date spans.

## What it produces

Output is split across rotated files, mirroring `logrotate` (recent rotations plain, older ones
gzipped) so PLV's discovery + gzip handling are exercised:

```
logs/sample/
  mail.log        # most recent ~7 days  (plain, live-tailed by PLV)
  mail.log.1      # prior week           (plain)
  mail.log.2.gz   # older                (gzip)
  mail.log.3.gz   # older                (gzip)
  mail.log.4.gz   # oldest               (gzip)
```

Each message is built from the shapes PLV's parser (`parser.go`) keys on, so the data flows through
the **real** classification pipeline:

- **Delivered** — full PMG flow: external client → `smtpd` → `pmg-smtp-filter` (the misleading
  `status=sent (250 2.5.0 OK (<sid>))` hand-off) → `accept mail … (<qid>)` → re-injected outbound leg
  to the real destination. PLV merges the inbound and outbound legs into **one** row.
- **Spam** / **Virus** — high `SA score` then `moved … to spam|virus quarantine` (no outbound leg).
- **Blocked** — `block mail to …`.
- **Bounced** / **Deferred** — accepted, then the outbound leg `status=bounced` / `status=deferred`.
- **Rejected** — standalone `NOQUEUE: reject` lines (RBL, relay denied, user unknown, greylist).

A configurable share of messages also carry a `Subject:` (via a `header_checks` WARN line) and a
`Trusted TLS connection established` line, so those columns populate. Volume follows a diurnal +
weekday/weekend curve for a natural-looking activity chart.

## Verifying it parsed

Run PLV against the output and check the disposition tally matches the generator's summary:

```bash
go run . -logdir logs/sample &
curl -s localhost:8080/api/stats | jq '{total,sent,spam,blocked,rejected,bounced,deferred}'
```

## Notes

- Stdlib only (`math/rand/v2`, `compress/gzip`, …) — no extra dependencies, part of the `plv` module.
- Safe to re-run: it overwrites the files in the output directory each time.
- This is a developer tool; it is **not** compiled into the PLV binary or the container image.
