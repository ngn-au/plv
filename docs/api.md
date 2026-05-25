# HTTP API

PLV serves a small JSON API that the embedded web UI consumes. There is no separate API token scheme:
when authentication is enabled, the same session cookie that protects the UI protects the API
(unauthenticated API requests get `401` with a JSON body; unauthenticated page requests redirect to
`/login`).

All responses are `application/json` unless noted.

## `GET /api/records`

The main record search. It speaks the [DataTables server-side](https://datatables.net/manual/server-side)
protocol, so it takes `draw` / `start` / `length` paging parameters and echoes `draw` back.

**Query parameters**

| Parameter | Description |
|---|---|
| `draw` | Opaque sequence number, echoed back unchanged. |
| `start` | Offset of the first row to return (0-based). |
| `length` | Page size. Defaults to 25, capped at 500. |
| `search[value]` | Global full-text term, matched case-insensitively across all record fields and the formatted timestamp. |
| `order[0][column]` | Sort column index (see below). |
| `order[0][dir]` | `asc` or `desc` (default `desc`). |
| `f_from`, `f_to`, `f_client`, `f_relay`, `f_qid` | Per-field substring filters (case-insensitive). |
| `f_status` | Filter by effective disposition or raw status (exact, case-insensitive). |
| `f_tls` | `yes` (TLS present), `no` (no TLS), or empty (any). |

**Sort columns** (by index): `0` time, `1` queue id, `2` client, `3` from, `4` to, `5` size,
`6` status/disposition, `7` relay.

**Response**

```json
{
  "draw": 1,
  "recordsTotal": 1234,
  "recordsFiltered": 42,
  "data": [
    ["2026-01-02T00:13:26Z", "A1A1A1A101", "sender@example.com", "recipient@example.net",
     "12.3 KB", "spam", "sent (250 2.5.0 OK ...) · SA 8/5 · Quarantine/Mark Spam (Level 3)",
     "mail.example.net[192.0.2.10]:25", "client.example.com[198.51.100.7]",
     "<spam001@example.org>", "Cheap meds", "TLSv1.3"]
  ]
}
```

Each `data` row is a string array, in this column order:

`0` time · `1` queue id · `2` from · `3` to · `4` size · `5` **disposition (badge)** ·
`6` **badge tooltip** (raw status + scanner summary) · `7` relay · `8` client · `9` message-id ·
`10` subject · `11` TLS.

`recordsTotal` counts visible (non-subsumed) records; `recordsFiltered` counts those matching the
current filters. Records merged into another item (subsumed outbound legs) are never returned as their
own row.

## `GET /api/detail?qid=<queue-id>`

Full detail for one message. `qid` may be the inbound, outbound, or synthetic queue id — all resolve
to the merged item. Returns `400` if `qid` is missing, `404` if unknown.

```json
{
  "timestamp": "2026-01-02T00:13:26Z",
  "queue_id": "A1A1A1A101",
  "from": "sender@example.com",
  "to": "recipient@example.net",
  "subject": "Cheap meds",
  "size": "12.3 KB",
  "size_bytes": 12595,
  "message_id": "<spam001@example.org>",
  "status": "sent",
  "status_detail": "(250 2.5.0 OK (C0FFEE0000000001))",
  "relay": "127.0.0.1[127.0.0.1]:10024",
  "client": "client.example.com[198.51.100.7]",
  "tls": "",
  "disposition": "spam",
  "filter": "pmg-smtp-filter",
  "filter_action": "spam quarantine",
  "spam_score": "8/5",
  "filter_rule": "Quarantine/Mark Spam (Level 3)",
  "filter_id": "C0FFEE0000000001",
  "delivery_queue_id": "",
  "lines": [
    { "timestamp": "2026-01-02T00:13:25Z", "raw": "...full raw log line..." }
  ]
}
```

`lines` is the message's full, de-duplicated, time-ordered raw log timeline — including the correlated
content-filter / rspamd lines.

## `GET /api/stats`

Aggregate counters and top-N lists computed over all visible records.

```json
{
  "total": 1234,
  "sent": 1000, "spam": 120, "blocked": 8, "rejected": 90,
  "bounced": 10, "deferred": 4, "other": 2,
  "top_senders":    [{ "name": "sender@example.com", "count": 73 }],
  "top_recipients": [{ "name": "recipient@example.net", "count": 64 }],
  "hourly":         [{ "hour": "2026-01-02 00", "count": 41 }],
  "ready": true,
  "status": "ready"
}
```

`top_senders` / `top_recipients` are the top 10 by count; `hourly` is the per-hour message volume.

## `GET /api/status`

Lightweight readiness probe — cheap enough to poll while the initial parse runs (the UI polls it, and
it is excluded from request logging).

```json
{ "ready": false, "status": "parsing mail.log.3.gz (2/9)", "records": 5821 }
```

`ready` becomes `true` once the initial parse completes; `status` reflects the current parse phase or
`ready`; `records` is the live count.

## Auth endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/login` | `GET` | Serves the login page. |
| `/login` | `POST` | Form `username` + `password`; on success sets the session cookie and redirects to `/`, otherwise back to `/login?error=1`. |
| `/logout` | `GET` | Deletes the session and clears the cookie. |

These exist whether or not auth is enabled, but only gate access when `AUTH_USERNAME` +
`AUTH_PASSWORD_HASH` are set.
