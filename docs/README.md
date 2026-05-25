# PLV documentation

Pick a starting point based on what you're trying to do.

## Get started

- **[Installation](installation.md)** — Run PLV with the published container, with Compose, or from
  source. Mounting the log directory, enabling authentication, enabling PostgreSQL persistence.
- **[Configuration](configuration.md)** — Every command-line flag and environment variable, grouped
  and explained.

## Understand

- **[Disposition model](disposition.md)** — The conceptual heart of PLV: how a message's *effective
  disposition* is derived, how `pmg-smtp-filter` and rspamd verdicts are correlated back to a
  message, how the inbound and outbound legs of a scanned message become one item, and how NOQUEUE
  rejections are handled.
- **[Architecture](architecture.md)** — How the Go files fit together and the parse → store → serve
  pipeline, including the live tail and optional persistence.

## Reference

- **[HTTP API](api.md)** — The JSON endpoints behind the UI: the DataTables-protocol record search,
  the message detail, the stats, and the readiness status.
- **[Postfix setup](postfix-setup.md)** — Enable `Subject:` logging with a `header_checks` rule.
- **[Security](security.md)** — Production-hardening checklist for a deployment that exposes mail
  metadata.

## Develop

- **[Development](development.md)** — Building, the test workflow, the checks CI runs, and how to add
  a new parser case or disposition.

---

**Quick orientation:**

- Just want it running? Start with [Installation](installation.md).
- A delivered message shows as spam (or vice versa) and you want to know why? Read
  [Disposition model](disposition.md).
- Subject column is empty? [Postfix setup](postfix-setup.md).
- Building a script against the JSON API? [HTTP API](api.md).
