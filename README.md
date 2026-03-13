# PLV — Postfix Log Viewer

See [https://hub.docker.com/r/jseifeddine/plv](https://hub.docker.com/r/jseifeddine/plv)

A lightweight web UI for searching and visualising Postfix mail logs.
Parses every `mail.log*` file (including gzipped rotations) on startup, then
live-tails `mail.log` for new entries. Optionally persists records to PostgreSQL
so data survives container restarts.

## Quick start

### Using the pre-built image (recommended)

```yaml
# docker-compose.yaml
services:
  plv:
    image: jseifeddine/plv:latest
    container_name: plv
    volumes:
      - /var/log:/var/log:ro
    restart: unless-stopped
    command: ["-addr", ":8080", "-logdir", "/var/log"]
```

```bash
docker compose up -d
```

### Building from source

```yaml
services:
  plv:
    build: .
    container_name: plv
    volumes:
      - /var/log:/var/log:ro
    restart: unless-stopped
    command: ["-addr", ":8080", "-logdir", "/var/log"]
```

```bash
docker compose up -d --build
```

The UI is available on the port/network defined in your `docker-compose.yaml`.

## PostgreSQL persistence

By default PLV keeps everything in memory — fast, but data is lost when the
container restarts. Set `DATABASE_URL` to enable PostgreSQL persistence:

```yaml
services:
  postgres:
    image: postgres:17-alpine
    container_name: plv-db
    environment:
      POSTGRES_DB: plv
      POSTGRES_USER: plv
      POSTGRES_PASSWORD: plv
    volumes:
      - plv_pgdata:/var/lib/postgresql/data
    restart: unless-stopped

  plv:
    image: jseifeddine/plv:latest
    # build: .              # uncomment to build from source instead
    container_name: plv
    depends_on:
      - postgres
    volumes:
      - /var/log:/var/log:ro
    environment:
      DATABASE_URL: postgres://plv:plv@plv-db:5432/plv?sslmode=disable
    restart: unless-stopped
    command: ["-addr", ":8080", "-logdir", "/var/log"]

volumes:
  plv_pgdata:
```

When `DATABASE_URL` is set, PLV will:

1. Connect to PostgreSQL on startup (retries for up to 30 seconds).
2. Create the `mail_records` table and indexes automatically.
3. Load any previously stored records into memory.
4. Persist every new or updated record as it is parsed.

If `DATABASE_URL` is not set, PLV runs purely in-memory with no database
dependency.

### Data retention

Without PostgreSQL, data lifetime is naturally bounded by the log files on
disk — PLV only ever sees what's in the current `mail.log*` files.

With PostgreSQL enabled, records accumulate indefinitely unless you set a
retention period:

```yaml
environment:
  DATABASE_URL: postgres://plv:plv@plv-db:5432/plv?sslmode=disable
  RETENTION_DAYS: 90
```

When `RETENTION_DAYS` is set, PLV will:

- Purge records older than the specified number of days on startup.
- Re-check and purge every hour while running.
- Remove expired records from both in-memory and the database.

If `RETENTION_DAYS` is not set, no automatic purging occurs.

## Authentication

Access is protected by a username and bcrypt-hashed password passed as
environment variables. Generate a hash with the built-in helper:

```bash
# Inside the container
docker exec plv plv hash 'my-secret-password'

# Or locally if you have the binary
./plv hash 'my-secret-password'
```

The command prints a bcrypt hash like:

```
$2a$10$fz/dpncQjZSE3BCSMLAp8.EEpgg101NIhp2SMO829miomGFLs.lYm
```

Set both variables in your compose environment (or a `.env` file):

```yaml
environment:
  AUTH_USERNAME: admin
  AUTH_PASSWORD_HASH: "$$2a$$10$$fz/dpncQjZSE3BCSMLAp8.EEpgg101NIhp2SMO829miomGFLs.lYm"
```

> **Note:** Dollar signs must be doubled (`$$`) inside `docker-compose.yaml` to
> prevent variable interpolation. Alternatively, place the raw hash in a `.env`
> file and reference it with `${AUTH_PASSWORD_HASH}`.

If neither variable is set, authentication is disabled entirely.

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | No | PostgreSQL connection string. Omit to run in-memory only. |
| `RETENTION_DAYS` | No | Number of days to keep records when using PostgreSQL. Omit to keep data indefinitely. |
| `AUTH_USERNAME` | No | Login username. Both `AUTH_USERNAME` and `AUTH_PASSWORD_HASH` must be set to enable auth. |
| `AUTH_PASSWORD_HASH` | No | Bcrypt hash of the login password. |

## Enabling subject logging in Postfix

Postfix does **not** log the `Subject:` header by default.
Without it the *Subject* field in PLV will always be empty.

To enable it, add a `header_checks` rule that makes Postfix log every
`Subject:` line it sees:

```bash
# 1. Add this to /etc/postfix/main.cf (if not already present)
header_checks = regexp:/etc/postfix/header_checks

# 2. Create or append to /etc/postfix/header_checks
echo '/^Subject:/     WARN' >> /etc/postfix/header_checks

# 3. Reload Postfix
systemctl reload postfix
```

After the reload, mail.log entries will include lines like:

```
postfix/cleanup[12345]: ABC123: warning: header Subject: Invoice #4021 from sender@example.com; ...
```

PLV picks these up automatically — no restart needed.
