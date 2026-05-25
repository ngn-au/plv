# Security Policy

PLV reads mail logs, which contain personal data (senders, recipients, subjects, client IPs), and
optionally guards access behind a password and stores records in PostgreSQL. Security reports are
taken seriously and handled with priority.

## Supported versions

The latest released `1.x` line receives security fixes. Older minor versions are not backported —
upgrade to the latest release.

| Version | Supported |
| ------- | --------- |
| 1.x     | ✅        |
| < 1.0   | ❌        |

## Reporting a vulnerability

**Please do not open a public issue for a security vulnerability.**

Report privately through GitHub's
[**Report a vulnerability**](https://github.com/ngn-au/plv/security/advisories/new) flow
(the **Security** tab → *Report a vulnerability*). This opens a private advisory visible only to
you and the maintainer.

When you report, please include:

- A description of the issue and its impact (authentication bypass, log/data exposure, SQL
  injection, SSRF, etc.).
- Steps to reproduce, a proof of concept, or the affected code path.
- The version or commit you tested against.

**Never paste real mail-log lines, recipient addresses, or other production data into a report.**
Reproduce with synthetic data — `example.com` addresses and RFC 5737 documentation IPs
(`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), exactly as the test fixtures do.

You can expect an acknowledgement within a few days. Once a fix is ready, a patched release is cut
and the advisory is published with credit to the reporter (unless you prefer to remain anonymous).

## Scope

In scope: authentication or session bypass, exposure of stored records to an unauthenticated user,
SQL injection in the persistence layer, denial of service triggered by crafted log input, and any
leak of secrets the process holds (`AUTH_PASSWORD_HASH`, `DATABASE_URL`).

Out of scope: findings that require an already-compromised host or database; running PLV with
authentication disabled (the documented default for trusted networks); and reports generated solely
by automated scanners without a demonstrated impact. See [`docs/security.md`](./docs/security.md)
for the production-hardening checklist.

## Operator note

PLV mounts the host log directory **read-only** and never writes to it. Treat the PLV UI, and any
PostgreSQL database backing it, as containing the same sensitive data as the mail logs themselves:
put the UI behind authentication and TLS, and restrict network access to it.
