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
| `plv wizard` | Interactive setup for distributed mode (receiver **or** forwarder), using `PLV_DATA_DIR`. |
| `plv pki init` | Receiver: create the CA (`ca.pem`/`ca-key.pem`) and the enrolment `ticket-salt` in the data dir. |
| `plv pki server <host>…` | Receiver: issue the server certificate for the given hostnames/IPs (SANs the forwarders connect to). |
| `plv pki ticket --cn <name>` | Receiver: mint an enrolment ticket for a forwarder named `<name>`. |
| `plv enroll --to <url> --cn <name> --ticket <t> --ca <ca.pem>` | Forwarder: enrol against the receiver (verified by its CA cert, copied out-of-band) and write the client certificate to the data dir. |

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | No | _(unset)_ | PostgreSQL connection string (e.g. `postgres://user:pass@host:5432/db?sslmode=disable`). Enables persistence. Omit to run in memory only. |
| `RETENTION_DAYS` | No | _(unset)_ | Positive integer. With persistence enabled, purge records older than this many days on startup and hourly. Omit to keep data indefinitely. Must be ≥ 1 — an invalid value is a fatal startup error. |
| `AUTH_USERNAME` | No | _(unset)_ | Login username. Both this and `AUTH_PASSWORD_HASH` must be set to enable authentication. |
| `AUTH_PASSWORD_HASH` | No | _(unset)_ | Bcrypt hash of the login password, from `plv hash`. |
| `AUTH_DISABLE` | No | _(unset)_ | Set truthy to run with **no** authentication. Otherwise a **receiver** with no `AUTH_*` set auto-generates an `admin` login and prints the password once to the log on first start. |
| `PLV_POSTFIX_CONF` | No | _(unset)_ | Path to a read-only `/etc/postfix` directory. PLV derives `mynetworks` and the local/hosted domains from it to classify mail **direction** more accurately, and re-derives them when the config changes. See [Direction & the Postfix config](#direction--the-postfix-config). |
| `PLV_INGEST_ADDR` | No | _(unset)_ | **Receiver**: listen address for the mTLS ingest endpoint (e.g. `:8443`). Setting this turns the instance into a receiver. Mutually exclusive with `PLV_FORWARD_TO`. |
| `PLV_FORWARD_TO` | No | _(unset)_ | **Forwarder**: the receiver URL to ship records to (e.g. `https://receiver.example.net:8443`). Setting this makes the instance a headless forwarder (no UI). Mutually exclusive with `PLV_INGEST_ADDR`. |
| `PLV_DATA_DIR` | No | `/data` | Directory for the PKI / enrolment state (receiver: CA, server cert, ticket salt, auto-auth; forwarder: client cert, ship checkpoint). Only used in distributed mode — standalone needs nothing here. |

## Distributed mode

By default PLV is a single process that reads a log directory and serves the UI. For multiple mail
servers, run it as a **receiver** (`PLV_INGEST_ADDR`) and one **forwarder** per server
(`PLV_FORWARD_TO`); the forwarders ship merged records over mutually-authenticated TLS and the
receiver shows one combined view, with each server identified by its client-cert CN in the **Server**
column. Use `plv wizard` (or the `plv pki …` / `plv enroll` subcommands) to set up the CA and enrol
forwarders. The data dir (`PLV_DATA_DIR`) holds the PKI material and **must persist** on the receiver
and should persist on each forwarder. A step-by-step deployment with systemd units is in
[dev-test-stack.md](./dev-test-stack.md).

## Direction & the Postfix config

PLV labels each message **inbound / outbound / internal / relayed** from the logs alone. When it
can't tell "ours" from transit by IP, point `PLV_POSTFIX_CONF` at a read-only `/etc/postfix`: PLV
parses `main.cf` (expanding `$vars`, following `hash:`/`cidr:`/`lmdb:` **file** lookups; database
tables are skipped) for two facts —

- **mynetworks** — a client inside it is a trusted source (a gateway's own backend, not inbound), and
  a relay into it is internal delivery, not an external send. Over-broad entries (`>/16`) are dropped.
- **local/hosted domains** — `from` a local domain → outbound; `to` a local domain → inbound.

It re-derives on change. In distributed mode each forwarder ships its own derived config, so the
receiver classifies each server's mail with that server's `mynetworks`/domains. The settings PLV is
using are shown on the in-app **Servers** page. PMG stores its domain list under `/etc/pmg`; mount
that too if you want exact domain matching (otherwise `mynetworks` carries the classification).

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
