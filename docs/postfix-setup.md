# Postfix setup: logging the Subject

Postfix does **not** log the `Subject:` header by default. Without it, the *Subject* column in PLV is
always empty. This is the one optional change you make on the **mail host** (not on PLV) to populate
it.

It works by adding a `header_checks` rule that tells Postfix to log a `WARN` line for every `Subject:`
header it sees. PLV picks those lines up automatically and attaches the subject to the matching
message.

## Steps

```bash
# 1. Point Postfix at a header_checks file (if it isn't already).
#    Add this line to /etc/postfix/main.cf:
header_checks = regexp:/etc/postfix/header_checks

# 2. Add the rule that logs every Subject: line.
echo '/^Subject:/     WARN' >> /etc/postfix/header_checks

# 3. Reload Postfix.
systemctl reload postfix
```

After the reload, `mail.log` entries will include lines such as:

```
postfix/cleanup[12345]: ABC123: warning: header Subject: Invoice #4021 from sender@example.com; ...
```

PLV parses these and fills in the subject — no PLV restart needed; new mail will show subjects, and a
restart re-parses history.

## Notes & caveats

- **`WARN`, not `REJECT`.** The `WARN` action only logs — it does not affect mail flow. Do not use a
  rejecting action here.
- **Privacy.** This writes every subject line into `mail.log`. Subjects are personal data; treat the
  logs (and PLV, and any database behind it) accordingly — see [Security](security.md).
- **Existing `header_checks`.** If you already have a `header_checks` file with other rules, just
  append the `/^Subject:/ WARN` line; don't replace the file.
- **Verify.** Send yourself a test message and confirm a `header Subject:` line appears in `mail.log`;
  it should then show up in PLV's Subject column.
