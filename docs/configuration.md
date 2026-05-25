# Configuration

PLV is configured by a few command-line flags and a few environment variables. Everything is
optional except the log directory (which defaults to `/var/log`).

## Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8080` | Listen address for the HTTP server. `:8080` listens on all interfaces; `127.0.0.1:8080` binds to localhost only. |
| `-logdir` | `/var/log` | Directory containing the Postfix `mail.log*` files. PLV reads it recursively for `mail.log*` and, if present, `rspamd/rspamd.log*`. |

In a container these are passed as the command, e.g. `command: ["-addr", ":8080", "-logdir", "/var/log"]`.

### Subcommands

| Invocation | Description |
|---|---|
| `plv hash <password>` | Print a bcrypt hash for `<password>`, for use as `AUTH_PASSWORD_HASH`. |
| `plv version` | Print the build version and exit. |

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | No | _(unset)_ | PostgreSQL connection string (e.g. `postgres://user:pass@host:5432/db?sslmode=disable`). Enables persistence. Omit to run in memory only. |
| `RETENTION_DAYS` | No | _(unset)_ | Positive integer. With persistence enabled, purge records older than this many days on startup and hourly. Omit to keep data indefinitely. Must be ≥ 1 — an invalid value is a fatal startup error. |
| `AUTH_USERNAME` | No | _(unset)_ | Login username. Both this and `AUTH_PASSWORD_HASH` must be set to enable authentication. |
| `AUTH_PASSWORD_HASH` | No | _(unset)_ | Bcrypt hash of the login password, from `plv hash`. |

## Persistence

`DATABASE_URL` is the only switch. When it is set, on startup PLV:

1. Opens the connection pool (max 10 open / 5 idle, 5-minute lifetime) and pings, retrying once a
   second for up to 30 seconds.
2. Runs `Migrate()` — `CREATE TABLE IF NOT EXISTS mail_records (…)` plus additive `ALTER TABLE … ADD
   COLUMN IF NOT EXISTS` for the scanner/disposition columns, and the indexes.
3. Loads all stored rows into the in-memory store (skipping rows merged into another item).
4. Upserts every record it parses or updates thereafter (batched, `ON CONFLICT (queue_id) DO UPDATE`).

When `DATABASE_URL` is unset, none of the above happens and there is no database dependency.

### Retention

`RETENTION_DAYS` only does anything alongside `DATABASE_URL` (in memory-only mode, data lifetime is
already bounded by the log files on disk). When set:

- Records with a timestamp older than `now − RETENTION_DAYS` are purged on startup and then once an
  hour, from **both** the in-memory store and the database.

## Authentication

Set **both** `AUTH_USERNAME` and `AUTH_PASSWORD_HASH` to require a login. With either unset,
authentication is disabled and the UI is open — only acceptable on a trusted network.

Generate the hash:

```bash
plv hash 'my-secret-password'        # local binary
docker run --rm ghcr.io/ngn-au/plv:latest hash 'my-secret-password'
```

Sessions are server-side, opaque 32-byte tokens stored in an `HttpOnly`, `SameSite=Lax` cookie with a
24-hour lifetime; expired sessions are swept periodically. Logging out deletes the session and clears
the cookie.

### Escaping the hash in Docker Compose

A bcrypt hash contains `$` characters, which Docker Compose treats as variable interpolation. Either
**double every `$`** when writing the literal in `docker-compose.yaml`:

```yaml
    environment:
      AUTH_USERNAME: admin
      AUTH_PASSWORD_HASH: "$$2a$$10$$fz/dpncQjZSE3BCSMLAp8.EEpgg101NIhp2SMO829miomGFLs.lYm"
```

…or put the raw (single-`$`) hash in a `.env` file and reference it:

```env
# .env
AUTH_PASSWORD_HASH=$2a$10$fz/dpncQjZSE3BCSMLAp8.EEpgg101NIhp2SMO829miomGFLs.lYm
```

```yaml
    environment:
      AUTH_PASSWORD_HASH: ${AUTH_PASSWORD_HASH}
```

## Log directory layout

PLV discovers, inside `-logdir`:

- `mail.log`, `mail.log.1`, `mail.log.2.gz`, … — the Postfix logs (gzipped rotations supported).
  Parsed oldest-first on startup; the active `mail.log` is then live-tailed.
- `rspamd/rspamd.log*` or `rspamd.log*` — optional. If present, rspamd verdicts are correlated onto
  the mail records by queue id (and the active file is tailed too).

Mount the directory **read-only** (`-v /var/log:/var/log:ro`). PLV never writes to it.
