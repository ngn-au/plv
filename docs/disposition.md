# The disposition model

This is the conceptual heart of PLV. Postfix's raw `status=` is often misleading once a content
filter is in the path; PLV computes an **effective disposition** for each message and shows that as
the badge, while preserving the raw status in the tooltip and detail view.

> All examples below use synthetic data — `example.com` addresses, RFC 5737 documentation IPs, and
> made-up queue ids — exactly as the test fixtures do. Never substitute real log lines.

## Why raw status lies

When Postfix hands a message to a local content filter (Proxmox Mail Gateway's `pmg-smtp-filter`), it
records the hand-off as a **successful delivery to the filter**:

```
postfix/lmtp[1002]: A1A1A1A101: to=<recipient@example.net>, relay=127.0.0.1[127.0.0.1]:10024,
  dsn=2.5.0, status=sent (250 2.5.0 OK (C0FFEE0000000001))
```

`status=sent` here means "the filter accepted the message", **not** "the recipient got it". If the
filter then quarantines the message as spam, a naive log viewer still shows green `sent`. PLV does
not.

## The effective dispositions

| Disposition | Meaning |
|---|---|
| `sent` | Delivered / relayed, or accepted clean by the local filter. |
| `spam` | Moved to spam quarantine, or rejected as "Blocked by SpamAssassin", or rspamd `reject`/`add header`/`rewrite subject`. |
| `blocked` | Filter explicitly blocked the message. |
| `virus` | Virus quarantine. |
| `rejected` | Rejected — including SMTP-time `NOQUEUE` rejections (RBL, relay, postscreen, policy). |
| `bounced` | Standard Postfix bounce. |
| `deferred` | Standard Postfix deferral, or rspamd `greylist`/`soft reject`. |
| `received` | Seen, but no terminal status yet. |

The raw Postfix `Status`/`StatusDetail` are kept untouched on the record; only `Disposition` (and the
`Filter` / `FilterAction` / `SpamScore` / `FilterRule` fields) are derived.

## Correlating the `pmg-smtp-filter` verdict

The filter logs its own lines under a 14+ hex-digit **session id** (distinct from a Postfix queue
id), for example:

```
pmg-smtp-filter[1001]: C0FFEE0000000001: new mail message-id=<spam001@example.org>
pmg-smtp-filter[1001]: C0FFEE0000000001: SA score=8/5 time=0.5 bayes=0.00 hits=...
pmg-smtp-filter[1001]: C0FFEE0000000001: moved mail for <recipient@example.net> to spam quarantine
  - B2B2B2B202 (rule: Quarantine/Mark Spam (Level 3))
```

PLV accumulates a per-session **verdict** (SA score, then the terminal `accept` / `quarantine` /
`block` action and its rule). The Postfix hand-off line carries that same session id in its trailing
parentheses — `status=sent (250 2.5.0 OK (C0FFEE0000000001))` — so PLV links the two and applies the
verdict to the Postfix record. The quarantine example above yields `Disposition = spam`,
`SpamScore = 8/5`, `FilterRule = Quarantine/Mark Spam (Level 3)`, even though Postfix said `sent`.

## One item per message: merging the two legs

A scanned-and-**accepted** message exists as two Postfix queue ids:

1. the **inbound** leg — Postfix → filter (`relay=127.0.0.1`), which holds the rich metadata
   (subject, original client, the filter session id);
2. the **outbound** leg — the filter re-injects the message under a *new* queue id for final
   delivery to the real destination.

The filter's `accept mail to <…> (<queue-id>) (rule: …)` line names that onward queue id. PLV records
it as the inbound record's `DeliveryQueueID` and **merges** the outbound leg into the inbound one
(`mergeDeliveryLeg`): the merged item keeps the inbound metadata and gains the real destination relay
and the final delivery status. The outbound leg is marked `Subsumed` so it is not listed separately.
Either queue id resolves to the merged item in search and detail.

Consequence: the `127.0.0.1` scanner leg only ever appears on its own when the message was **not**
delivered onward — i.e. it was quarantined or blocked.

## rspamd hosts

On a host where rspamd logs verdicts to its own `rspamd.log` (not `mail.log`), each task-summary line
carries the Postfix queue id:

```
rspamd[2002]: ...; rspamd_task_write_log: id: <...>, qid: <D3D3D3D304>,
  ... (default: F (add header): [7.50/15.00] ...)
```

PLV parses the action and score and **correlates** the verdict onto the existing mail record with
that queue id. It never creates a standalone record from an rspamd line. Action mapping:

| rspamd action | Disposition |
|---|---|
| `reject`, `add header`, `rewrite subject` | `spam` |
| `soft reject`, `greylist` | `deferred` |
| `no action` (and anything else) | unchanged (keeps the Postfix-derived disposition) |

Because the verdict can be written just before PLV finalizes the mail record in live mode, an rspamd
verdict whose record hasn't been seen yet is held briefly (pending) and applied when the record
arrives; pending verdicts older than ~15 minutes are pruned.

## NOQUEUE rejections

SMTP-time rejections (RBL, relay denial, postscreen, sender/recipient policy) are refused *before* a
queue id is assigned, so they never get one and never get a `removed` line:

```
postfix/smtpd[3003]: NOQUEUE: reject: RCPT from unknown[192.0.2.50]: 554 5.7.1
  Service unavailable; Client host [192.0.2.50] blocked using zen.spamhaus.org; from=<...> to=<...>
```

PLV captures each as a standalone record with a deterministic **synthetic** queue id (an `N`-prefixed
FNV hash of the line, so re-parsing dedupes rather than duplicating) and always classifies it as
`rejected` — even when Postfix used a 4xx code, because the message was refused, not merely deferred.

## Records with no filter verdict

For ordinary messages with no content-filter involvement, `deriveDisposition` maps the raw Postfix
status: `sent`→`sent`, `bounced`→`bounced`, `deferred`→`deferred`, and `reject` →
`spam`/`virus`/`rejected` depending on whether the detail mentions SpamAssassin/spam or virus (this
catches milter rejections such as "Blocked by SpamAssassin" that reach `mail.log`).

## When you change this model

Adding or changing an outcome means updating, together:

- `deriveDisposition` (Postfix-only path) and/or `rspamdDisposition` (rspamd path) in the source;
- the stats bucketing in `Store.Stats` (so the new outcome is counted correctly);
- this document and the table in the [README](../README.md#spam--mail-filter-detection);
- a test in [`parser_test.go`](../parser_test.go) using synthetic data.
