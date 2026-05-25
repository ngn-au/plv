# Screenshots

UI screenshots used in the [project README](../README.md). Each view ships a **light** and a **dark**
variant; the README auto-selects via `<picture>` + `prefers-color-scheme`.

All screenshots are rendered from **fully synthetic data** produced by
[`tools/loggen`](../tools/loggen) — `example.*` domains and RFC 5737 documentation IPs, never real
mail logs.

## Dashboard

Disposition stat cards, hourly message volume, status-distribution donut, top senders/recipients, and
the live searchable message table.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="dashboard-dark.png" />
  <img src="dashboard-light.png" alt="PLV dashboard (light/dark)" width="100%" />
</picture>

| Light | Dark |
|---|---|
| [`dashboard-light.png`](dashboard-light.png) | [`dashboard-dark.png`](dashboard-dark.png) |

## Message detail

Effective disposition, sender / recipient / scanner breakdown, the raw Postfix status, and the full
correlated log timeline (Postfix + `pmg-smtp-filter`).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="mail-item-dark.png" />
  <img src="mail-item-light.png" alt="PLV message detail (light/dark)" width="100%" />
</picture>

| Light | Dark |
|---|---|
| [`mail-item-light.png`](mail-item-light.png) | [`mail-item-dark.png`](mail-item-dark.png) |
