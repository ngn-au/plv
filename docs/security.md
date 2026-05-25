# Security & hardening

PLV exposes mail metadata — senders, recipients, subjects, client IPs, spam scores. Treat a PLV
deployment, and any PostgreSQL database behind it, as carrying the same sensitivity as the mail logs
themselves. This is the production-hardening checklist; to report a vulnerability, see
[`../SECURITY.md`](../SECURITY.md).

## Production checklist

- [ ] **Mount logs read-only.** Always `-v /var/log:/var/log:ro`. PLV never writes to the log
      directory; the read-only mount enforces that.
- [ ] **Enable authentication.** Set `AUTH_USERNAME` + `AUTH_PASSWORD_HASH` (from `plv hash`). With
      either unset, the UI is open to anyone who can reach it.
- [ ] **Put TLS in front.** PLV serves plain HTTP. Terminate TLS at a reverse proxy (nginx, Caddy,
      Traefik) and only expose the proxy. Bind PLV itself to localhost or an internal network
      (`-addr 127.0.0.1:8080`, or no published port with the proxy on the same Docker network).
- [ ] **Restrict network access.** The UI should not be on the public internet without auth + TLS +
      ideally an IP allowlist or VPN. Don't publish the container port directly to `0.0.0.0` in an
      untrusted environment.
- [ ] **Protect the database.** Use a strong `POSTGRES_PASSWORD`, keep Postgres on a private network,
      and prefer `sslmode=require` (or better) when the DB is not on the same host.
- [ ] **Set a retention policy.** With persistence enabled, set `RETENTION_DAYS` so you don't hold
      mail metadata longer than you need to. Memory-only mode is naturally bounded by the logs on
      disk.
- [ ] **Pin the image version.** Deploy `ghcr.io/ngn-au/plv:1.0.0`, not `:latest`, and upgrade
      deliberately.

## What PLV does by design

- **Read-only by nature.** PLV opens log files for reading; it has no endpoint that modifies the host
  or the mail system.
- **Server-side sessions.** Login issues an opaque 32-byte random token in an `HttpOnly`,
  `SameSite=Lax` cookie with a 24-hour lifetime. Passwords are checked against a bcrypt hash; the
  plaintext password is never stored.
- **Parameterised SQL.** All database access uses parameterised queries (`lib/pq` placeholders) — log
  content is never concatenated into SQL.
- **Defensive parsing.** Untrusted log lines are matched with bounded regexes; malformed lines are
  skipped, not fatal.
- **Tiny dependency surface.** Two third-party modules only (`github.com/lib/pq`,
  `golang.org/x/crypto`), which keeps the supply-chain surface small. Dependabot, `govulncheck`,
  CodeQL, dependency review, and OpenSSF Scorecard run in CI.

## What's not in scope

- **Multi-user / RBAC.** Auth is a single shared username/password, intended to keep casual access
  out — not to provide per-user accounts or audit. Front it with SSO at the proxy if you need more.
- **Rate limiting / brute-force lockout.** None is built in; put it at the reverse proxy if your
  exposure warrants it.

## Handling log data responsibly

Mail logs are personal data. When filing a bug or a security report, **never paste real log lines or
recipient addresses** — reproduce with synthetic data (`example.com` addresses, RFC 5737
documentation IPs), exactly as the test fixtures do.
