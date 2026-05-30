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
| `spam` | Moved to spam quarantine, rejected as "Blocked by SpamAssassin", or rspamd `reject`. (rspamd `add header`/`rewrite subject` only *tag* a message that is still delivered — they keep the delivered disposition; see below.) |
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
| `reject` | `spam` (the message was actually stopped) |
| `add header`, `rewrite subject` | unchanged — the message is only *tagged* (an `X-Spam` header / rewritten subject) and is still **delivered**, so the Postfix-derived disposition stands. The spam score still rides along on the record (shown in the Scanner panel), so the flagging stays visible. |
| `soft reject`, `greylist` | `deferred` |
| `no action` (and anything else) | unchanged (keeps the Postfix-derived disposition) |

This mirrors PLV's purpose: correct `status=sent` only when the scanner *quarantines or blocks*.
An `add header`/`rewrite subject` neither holds nor refuses the mail — it reaches the mailbox — so it
must not read as `spam`. (Earlier versions classified all three as `spam`, which made a delivered
message show both "spam" and "delivered to mailbox".)

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

## Message direction (inbound / outbound / internal / relayed)

Separate from disposition, PLV labels each message's **direction**, derived purely from log
facts so it generalises across standalone, milter, content-filter and relay-pair setups
(see [`direction.go`](../direction.go)). It uses three per-leg signals, aggregated across all
legs of a message (records sharing a Message-ID):

- **received-public-unauth** — the leg's `client=` IP is public *and* the leg carries no
  `sasl_username=`. The signature of mail arriving from the internet. (An authenticated
  submission from a public IP is a *user sending*, not inbound.)
- **sends-public** — the leg's `relay=` IP is public *and* it is not a local delivery
  (loopback content-filter hand-offs are private; `lmtp`/`dovecot`/`local`/`virtual` or a
  "… Saved" / "delivered to mailbox" detail are terminal, not an external send).
- **delivers-local** — a terminal local-mailbox delivery.

A fourth, **relay-leg**, is the key to telling transit apart from gateway delivery: a
*single* queue id that both received-public-unauth **and** sends-public, **and** is not a
content-filter re-injection. Re-injection is detected by `DeliveryQueueID` (the
`accept mail … (qid)` merge that links a scanner leg to its onward leg) or an `orig_client=`
on the leg. This matters because a content-filter gateway delivers **inbound** mail onward to
a backend mailserver that often has a *public* IP — which, once the scanner and onward legs
are merged, looks exactly like a public→public relay. Requiring a single un-merged leg avoids
mislabelling all such inbound mail as relayed (this was verified against the PMG sample, where
the naive rule mis-classed ~37k inbound messages).

The classification, in order:

| Direction | Rule |
|---|---|
| `relayed` | has a relay-leg **and** no local delivery — pure transit (external→us→external), e.g. a PBX/appliance on a public IP shipping notifications out through us to an external recipient. |
| `inbound` | received from the internet (and not pure relay) — including content-filter inbound to a public-IP backend, and a relay that *also* delivered locally. |
| `outbound` | sent to the internet (authenticated submission, or an internal client relaying out). |
| `internal` | neither — internal sender to internal/local recipient. |

Cross-platform caveat: direction rests on public-vs-private IPs, SASL auth and local-delivery
markers — never on hard-coded hostnames. The one inherent ambiguity is a site that relays
*inbound* mail to a backend over a **public** IP in a single un-filtered hop (no content
filter, no local delivery): from the logs alone that is indistinguishable from transit, and
reads as `relayed`. Content-filtered and private-backend setups are unaffected.

## When you change this model

Adding or changing an outcome means updating, together:

- `deriveDisposition` (Postfix-only path) and/or `rspamdDisposition` (rspamd path) in the source;
- the stats bucketing in `Store.Stats` (so the new outcome is counted correctly);
- this document and the table in the [README](../README.md#spam--mail-filter-detection);
- a test in [`parser_test.go`](../parser_test.go) using synthetic data.
