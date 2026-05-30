# Installation

PLV is a single static binary that reads a directory of Postfix `mail.log*` files and serves a web
UI. There is nothing to install on the mail host beyond giving PLV **read-only** access to the logs.

## Requirements

- A directory containing Postfix logs: `mail.log` plus any rotations (`mail.log.1`, `mail.log.2.gz`,
  …). On a typical host this is `/var/log`.
- One of: Docker (recommended), or Go 1.26+ to build/run from source.
- *(Optional)* a PostgreSQL server if you want records to survive restarts.

PLV opens the log directory read-only and never writes to it.

## Option 1 — Published container (recommended)

Multi-arch (`amd64` / `arm64`) images are published to the GitHub Container Registry on every
release:

```bash
docker pull ghcr.io/ngn-au/plv:latest   # or a pinned :1.0.1 / :1.0
```

Minimal run:

```bash
docker run -d --name plv \
  -p 8080:8080 \
  -v /var/log:/var/log:ro \
  ghcr.io/ngn-au/plv:latest \
  -addr :8080 -logdir /var/log
```

Open <http://localhost:8080>.

Tags:

| Tag | Points at |
|---|---|
| `latest` | The most recent release. |
| `1.0.1`, `1.0` | A pinned version / minor line. |
| `edge` | The current `main` branch (unreleased). |

## Option 2 — Docker Compose

The simplest stack — in-memory, no auth, for a trusted network:

```yaml
# docker-compose.yaml
services:
  plv:
    image: ghcr.io/ngn-au/plv:latest
    container_name: plv
    ports:
      - "8080:8080"
    volumes:
      - /var/log:/var/log:ro
    restart: unless-stopped
    command: ["-addr", ":8080", "-logdir", "/var/log"]
```

```bash
docker compose up -d
```

The repository's [`docker-compose.yaml`](../docker-compose.yaml) is a fuller example: PostgreSQL
persistence, authentication, a 90-day retention policy, and a pinned bridge network.

## Option 3 — From source

```bash
git clone https://github.com/ngn-au/plv.git
cd plv
go build -o plv .
./plv -addr :8080 -logdir /var/log
```

The web UI is embedded in the binary (`go:embed`), so the single `plv` file is all you need to
deploy.

## Enabling authentication

Authentication is **off** unless both `AUTH_USERNAME` and `AUTH_PASSWORD_HASH` are set. Generate a
bcrypt hash with the built-in helper, then pass both variables:

```bash
docker run --rm ghcr.io/ngn-au/plv:latest hash 'my-secret-password'
# → $2a$10$....   (use this as AUTH_PASSWORD_HASH)
```

```yaml
    environment:
      AUTH_USERNAME: admin
      AUTH_PASSWORD_HASH: "$$2a$$10$$....."   # doubled $$ in compose, or use a .env file
```

See [Configuration](configuration.md#authentication) for the dollar-sign escaping details.

## Enabling PostgreSQL persistence

Set `DATABASE_URL`. PLV connects on boot (retrying for up to 30s), creates the `mail_records` table
and its indexes, loads existing rows into memory, and persists every new/updated record:

```yaml
    environment:
      DATABASE_URL: postgres://plv:plv@plv-db:5432/plv?sslmode=disable
      RETENTION_DAYS: 90   # optional: purge rows older than 90 days
```

Without `DATABASE_URL`, PLV runs purely in memory — its view is bounded by whatever is still in the
`mail.log*` files on disk.

## Verifying

- The startup log prints `PLV <version> starting`, the parse progress per file, and
  `initial parse complete: N records`.
- `GET /api/status` returns `{"ready":true,…}` once the initial parse finishes.
- `plv version` (or `docker run --rm ghcr.io/ngn-au/plv version`) prints the build version.

## Upgrading

Pull the new image (or rebuild) and recreate the container. The PostgreSQL schema migrates itself on
boot with additive `ALTER TABLE … ADD COLUMN IF NOT EXISTS` statements, so upgrades are in place — no
manual migration step. Pin to a version tag in production and bump deliberately.
