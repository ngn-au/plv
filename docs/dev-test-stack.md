# Dev test stack — local distributed run (scratch notes)

> Working notes for spinning up a local **distributed** PLV stack against the sample
> logs, for manual UI testing. Not user-facing docs — revisit / fold into the real
> docs later. Uses a scratch dir under `/tmp` and never writes into the repo except
> reading the git-ignored `logs/` sample dirs.

## Topology

One **receiver** (UI + mTLS ingest, *empty* logdir so it has no logs of its own) and
**two forwarders** (`node-a`, `node-b`) that parse the sample logs and ship over mTLS.
A forwarder's Server name = its **client-cert CN**, stamped by the receiver — so both
`node-a` and `node-b` show in the Server column (no blanks).

```
receiver  (PLV_INGEST_ADDR=:8443, UI :8099, -logdir <empty>)
   ▲ mTLS
   ├── node-a    forwarder  (CN=node-a,   -logdir logs/node-a)
   └── node-b  forwarder  (CN=node-b, -logdir logs/node-b)
```

## One-time setup (PKI + enrolment)

```sh
REPO=$(pwd)   # run from the repo root
SM=/tmp/plv-stack
mkdir -p "$SM"/{receiver,node-a,node-b,emptylog}
cd "$REPO"
go build -o "$SM/plv" .

# Receiver CA + server cert (SANs the forwarders connect to)
PLV_DATA_DIR=$SM/receiver "$SM/plv" pki init
PLV_DATA_DIR=$SM/receiver "$SM/plv" pki server localhost 127.0.0.1

# A fixed login for testing (instead of the auto-generated receiver password)
"$SM/plv" hash 'plvdemo123' > "$SM/recv.hash"

# Mint a ticket per node and enrol each forwarder (writes its client cert)
TK_A=$(PLV_DATA_DIR=$SM/receiver "$SM/plv" pki ticket --cn node-a   | awk '/^ticket:/{print $2}')
TK_B=$(PLV_DATA_DIR=$SM/receiver  "$SM/plv" pki ticket --cn node-b | awk '/^ticket:/{print $2}')
PLV_DATA_DIR=$SM/node-a   "$SM/plv" enroll --to https://127.0.0.1:8443 --cn node-a   --ticket "$TK_A"
PLV_DATA_DIR=$SM/node-b "$SM/plv" enroll --to https://127.0.0.1:8443 --cn node-b --ticket "$TK_B"
```

## Start (rebuild + (re)launch)

```sh
REPO=$(pwd)   # run from the repo root
SM=/tmp/plv-stack
cd "$REPO"
pkill -f "$SM/plv" 2>/dev/null; sleep 1
go build -o "$SM/plv" .                       # re-embeds web/index.html on every change
: > "$SM/emptylog/mail.log"
rm -f "$SM/node-a/forward-state.json" "$SM/node-b/forward-state.json"  # re-ship full history
HASH=$(cat "$SM/recv.hash")

# Receiver: UI :8099, ingest :8443, no local logs
PLV_DATA_DIR=$SM/receiver PLV_INGEST_ADDR=:8443 \
  AUTH_USERNAME=admin AUTH_PASSWORD_HASH="$HASH" \
  nohup "$SM/plv" -addr :8099 -logdir "$SM/emptylog" > "$SM/recv.log" 2>&1 &

# Wait for ingest, then start both forwarders
for i in $(seq 1 40); do nc -z localhost 8443 2>/dev/null && break; sleep 0.5; done
PLV_DATA_DIR=$SM/node-a   PLV_FORWARD_TO=https://127.0.0.1:8443 \
  nohup "$SM/plv" -logdir "$REPO/logs/node-a"   > "$SM/node-a.log"   2>&1 &
PLV_DATA_DIR=$SM/node-b PLV_FORWARD_TO=https://127.0.0.1:8443 \
  nohup "$SM/plv" -logdir "$REPO/logs/node-b" > "$SM/node-b.log" 2>&1 &
```

## Access / verify

- UI: <http://127.0.0.1:8099> — login `admin` / `plvdemo123`
- Confirm both shipped: `grep -h shipped "$SM"/node-a.log "$SM"/node-b.log`
- Receiver total: tail `"$SM/recv.log"` for `ingest: … total=`

## Stop

```sh
pkill -f "/tmp/plv-stack/plv"
```

## Notes

- After **any** code change, rebuild (`go build -o "$SM/plv" .`) and relaunch — the UI
  is embedded via `go:embed`, so a running binary won't pick up `web/index.html` edits.
- The receiver is **in-memory** (no `DATABASE_URL`), so a restart drops its records;
  clearing the forwarder `forward-state.json` checkpoints makes them re-ship everything.
- `logs/node-a` and `logs/node-b` are git-ignored operator sample logs — read-only here.

---

# Running a prebuilt binary (standalone)

The default production posture is **one** PLV process that parses a log directory and
serves the UI — no mTLS, no ingest endpoint, no forwarder, no PKI.

Get a binary either by building from source or by copying it out of the published image:

```sh
# (a) build from source (needs Go 1.26)
go build -o plv .

# (b) or copy it out of the container image
id=$(docker create jseifeddine/plv:latest)
docker cp "$id:/usr/local/bin/plv" ./plv
docker rm "$id"
```

Run it against a directory of `mail.log*` files (read-only is the production posture):

```sh
./plv -addr :8080 -logdir /var/log            # http://localhost:8080

# auth is OFF unless you set BOTH of these (otherwise the UI is open):
./plv hash 'my-password'                       # prints a bcrypt hash
AUTH_USERNAME=admin AUTH_PASSWORD_HASH='<hash>' ./plv -addr :8080 -logdir /var/log

# point PLV at /etc/postfix (read-only) so it classifies mail direction with this
# host's mynetworks + local domains, not just the IP heuristic:
PLV_POSTFIX_CONF=/etc/postfix ./plv -addr :8080 -logdir /var/log
```

PLV parses every `mail.log*` (including gzipped rotations) on startup, then live-tails the
active `mail.log`. If an `rspamd/rspamd.log*` subdir is present it is auto-correlated. To
try it against the bundled rspamd sample: `./plv -addr :8088 -logdir logs/postfix_with_rspamd`.

## Data dir (`PLV_DATA_DIR`)

`PLV_DATA_DIR` (default `/data`) holds PLV's small persistent state. **What lives there —
and whether it must survive restarts — depends on the mode:**

| Mode | Uses the data dir? | Persist across restarts? |
|---|---|---|
| **Standalone** (just `-logdir`, the case above) | **No** — it is never touched. | **No.** Mail records are kept in memory and re-parsed from `-logdir` on every start, so there is nothing to persist. (For durable history independent of the logs, set `DATABASE_URL` — a separate Postgres store, not the data dir.) |
| **Receiver** (`PLV_INGEST_ADDR`) | Yes: the CA (`ca.pem`/`ca-key.pem`), its server cert (`cert.pem`/`key.pem`), the enrolment `ticket-salt`, and an auto-generated `auth.json` login. | **Yes — keep it.** Losing it breaks every forwarder's trust (they must re-enrol) and regenerates the admin password. |
| **Forwarder** (`PLV_FORWARD_TO`) | Yes: its client cert (`cert.pem`/`key.pem` + `ca.pem`) and `forward-state.json` (byte offsets already shipped). | **Recommended.** Without the cert it must re-enrol; without `forward-state.json` it re-ships the full history on restart (harmless — the receiver de-dupes — just redundant). |

So for a local **standalone** run there is nothing to persist: point `-logdir` at your logs
and go. Authentication, if you want it, is controlled entirely by the
`AUTH_USERNAME`/`AUTH_PASSWORD_HASH` env vars — not the data dir.

---

# Distributed deployment (systemd)

A production-style multi-host setup: one **receiver** (the UI + a mutually-authenticated
TLS ingest endpoint) and any number of **forwarders** (one per mail server, each tailing
its own `/var/log/mail.log` and shipping records to the receiver).

```
   mail host A ──┐
   plv-forwarder │  client cert (CN=A)
                 ├──► receiver :8443  (mTLS ingest)   ──►  UI :8080
   mail host B ──┘  client cert (CN=B)                     (one merged view)
   plv-forwarder
```

Trust model: the receiver runs a tiny CA. Each forwarder enrols once with a short-lived
**ticket** (an HMAC of its name) and receives a client certificate whose **CN is its node
name** — which becomes the value shown in the UI's *Server* column. Only the **ingest**
channel (`:8443`) is mTLS; the **UI** (`:8080`) is plain HTTP — put it behind a reverse
proxy for TLS, or bind it to localhost and tunnel.

## 0. Install the binary (every host)

```sh
# copy ./plv (built, or extracted from the image — see above) to each host:
sudo install -m 0755 plv /usr/local/bin/plv

# a dedicated unprivileged user + a persistent data dir for PKI/state:
sudo useradd --system --home-dir /var/lib/plv --shell /usr/sbin/nologin plv
sudo install -d -o plv -g plv -m 0750 /var/lib/plv
```

## 1. Receiver

Initialise the CA and issue the receiver's server cert for the name(s)/IP(s) the
forwarders will connect to (these become the cert SANs). Run the CLI **as the `plv` user**
so the files land owned correctly in the data dir:

```sh
sudo -u plv env PLV_DATA_DIR=/var/lib/plv plv pki init
sudo -u plv env PLV_DATA_DIR=/var/lib/plv plv pki server receiver.example.net 203.0.113.10
#   → writes the CA + server cert. Copy /var/lib/plv/ca.pem to each forwarder out-of-band;
#     enrolment verifies the receiver against it (no trust-on-first-use).

# An empty log dir so the receiver doesn't parse its own /var/log:
sudo install -d -o plv -g plv /var/lib/plv/empty
```

`/etc/systemd/system/plv-receiver.service`:

```ini
[Unit]
Description=PLV receiver (UI + mTLS ingest)
After=network-online.target
Wants=network-online.target

[Service]
User=plv
Group=plv
Environment=PLV_DATA_DIR=/var/lib/plv
Environment=PLV_INGEST_ADDR=:8443
# Optional persistence + fixed login. Without AUTH_*, an admin password is
# auto-generated into /var/lib/plv/auth.json on first start (printed once to the journal).
#   EnvironmentFile=/etc/plv/receiver.env   # DATABASE_URL=, AUTH_USERNAME=, AUTH_PASSWORD_HASH=
ExecStart=/usr/local/bin/plv -addr 127.0.0.1:8080 -logdir /var/lib/plv/empty
Restart=on-failure
RestartSec=2
# hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/plv

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now plv-receiver
sudo journalctl -u plv-receiver | grep -i password   # if you let it auto-generate auth
```

Open the ingest port to the forwarders only (the UI stays on localhost / behind a proxy):

```sh
sudo ufw allow from 203.0.113.0/24 to any port 8443 proto tcp   # adjust to your network
```

Mint one enrolment ticket per forwarder (CN = the node name you want in the UI):

```sh
sudo -u plv env PLV_DATA_DIR=/var/lib/plv plv pki ticket --cn mailhost-a
sudo -u plv env PLV_DATA_DIR=/var/lib/plv plv pki ticket --cn mailhost-b
#   → prints each ticket. Hand the ticket plus a copy of the receiver's ca.pem to that node.
```

## 2. Forwarder (repeat per mail host)

After step 0 on the mail host, enrol it (writes its client cert into the data dir). Use the
ticket from the receiver and the receiver's `ca.pem` (copied out-of-band in step 1) — the CA
verifies the receiver's identity on the enrol connection, with no trust-on-first-use:

```sh
sudo -u plv env PLV_DATA_DIR=/var/lib/plv plv enroll \
  --to https://receiver.example.net:8443 \
  --cn mailhost-a \
  --ticket '<ticket-from-the-receiver>' \
  --ca /etc/plv/ca.pem   # the receiver's CA cert, copied across

`/etc/systemd/system/plv-forwarder.service`:

```ini
[Unit]
Description=PLV forwarder (ships mail.log to the receiver over mTLS)
After=network-online.target
Wants=network-online.target

[Service]
User=plv
Group=plv
SupplementaryGroups=adm
Environment=PLV_DATA_DIR=/var/lib/plv
Environment=PLV_FORWARD_TO=https://receiver.example.net:8443
# Ship this server's mynetworks/local-domains so the receiver classifies its mail
# direction correctly. /etc/postfix is mounted read-only (ProtectSystem covers it).
Environment=PLV_POSTFIX_CONF=/etc/postfix
ExecStart=/usr/local/bin/plv -logdir /var/log
Restart=on-failure
RestartSec=5
# hardening — /var/log + /etc/postfix are read-only to the forwarder; only the data dir is writable
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/plv

[Install]
WantedBy=multi-user.target
```

`SupplementaryGroups=adm` grants read access to `/var/log/mail.log*` on Debian/Ubuntu
(adjust to your distro's log group). The forwarder is headless — no `-addr`, no UI.

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now plv-forwarder
sudo journalctl -u plv-forwarder -f       # expect "shipped N records" lines
```

> Prefer not to touch the CLI? `plv wizard` walks through receiver **or** forwarder setup
> interactively (`sudo -u plv env PLV_DATA_DIR=/var/lib/plv plv wizard`).

## 3. Verify

- Receiver journal shows `mode: receiver (ingest :8443, UI 127.0.0.1:8080)` and
  `ingest: … total=` climbing as forwarders connect.
- Forwarder journal shows `mode: forwarder → https://…` and periodic `shipped N records`.
- The UI's *Server* column lists each forwarder by its CN (`mailhost-a`, `mailhost-b`, …).

## 4. Operating notes

- **Data dir must persist on the receiver** (CA + server cert + `ticket-salt` + `auth.json`)
  and **should persist on each forwarder** (its client cert + `forward-state.json`). See the
  *Data dir* table above. Back up the receiver's `/var/lib/plv`.
- **UI is plain HTTP.** Front it with nginx/Caddy for TLS + access control, or keep it on
  `127.0.0.1` and reach it over SSH. mTLS protects ingest, not the UI.
- **Persistence of mail history:** set `DATABASE_URL` (PostgreSQL) on the receiver if you
  want records to survive a receiver restart; otherwise the merged view is in-memory and
  rebuilt as forwarders re-ship (clearing a forwarder's `forward-state.json` forces a full
  re-ship). Retention purging is enabled with `RETENTION_DAYS`.
- **Upgrades:** drop in the new binary and `systemctl restart`. A forwarder resumes from its
  `forward-state.json` offset; the receiver re-accepts (de-duped) snapshots.
- **Re-enrolling a node:** mint a fresh ticket on the receiver and re-run `plv enroll`; the
  CN (node name) can stay the same.
