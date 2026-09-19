# pushmails-client

Send your PushMails campaigns **from your own server**.

It connects to the central PushMails system, pulls the emails waiting to be
sent, delivers them and reports the results. The mail leaves your
infrastructure, from your IP address — the sending reputation is yours.

By default it is the mail server: it looks up the recipient's MX and talks to
it directly, with nothing else in the path. If you would rather keep an
existing relay in the middle, it can do that too.

- **Delivers by itself** — no Postfix, no Exim, no smarthost required
- **Zero dependencies** — the Go standard library only
- **Single binary** — build it, copy it, run it
- **One-way HTTP** — works behind NAT and a firewall, no inbound port needed
- **No lost work** — if the process crashes, the central system takes the jobs back

## Installation

```bash
git clone https://github.com/appstonia/pushmails-client.git
cd pushmails-client
go build -o pushmails-client ./cmd/pushmails-client
```

On Windows:

```powershell
go build -o pushmails-client.exe .\cmd\pushmails-client
```

Or build every release binary at once with `make release` — it cross-compiles
for Linux, macOS and Windows into `dist/`.

Requires Go 1.23+. You can copy the built binary to `/usr/local/bin/`
(`C:\Program Files\PushMails\` on Windows).

Check what you built with `pushmails-client -version`. The same string is sent
with every request to the central system, so support can tell which release an
installation is running without asking. A build made from a tagged checkout
reports that tag; any other build reports the release the source belongs to.

## Usage

Get a token from the PushMails panel via **Sender servers → Add server**. The
token is shown only once.

```bash
pushmails-client \
  --api https://api.pushmails.net \
  --token pm_live_xxxxxxxxxxxx \
  --ehlo mail.yourexample.com
```
Or with a config file:
```bash
pushmails-client \
  --config /etc/pushmails/client.cfg
```

Once running it connects to the central system and starts sending as soon as a
campaign begins. It is meant to stay running.

## Direct or relay

This is the one decision to make before anything else.

| | `direct` (default) | `relay` |
|---|---|---|
| Who delivers | this process, straight to the recipient's MX | the SMTP server you nominate |
| What you need | outbound port 25, a PTR record for your address | a working relay |
| What `sent` means | the recipient's server accepted the message | your relay accepted it |
| Bounces after acceptance | there are none — the answer is synchronous | arrive in the From mailbox; the panel never learns |
| Pacing, per-domain limits, back-off | handled here | your relay's business |

**Direct is the default because it is the point.** When a relay queues the
message, "sent" only means "written to a local spool" — the real answer comes
back minutes later as a bounce nobody is reading, and your campaign statistics
say delivered when nothing was. Talking to the recipient's server directly
makes the report the truth.

Relay mode is there for the setups that already have mail infrastructure and
have to keep it: a hardened Postfix, a smarthost your provider insists on, an
appliance that has to see outgoing mail. Pick it with `--mode relay`; the
`--smtp-*` options then apply and nothing else changes.

### What direct mode needs from your server

Two things, and the health checks will tell you if either is missing:

1. **Outbound port 25.** Many providers filter it on home connections and cheap
   VPS plans. Ask them to open it, or use relay mode through a server that has
   it.
2. **A reverse DNS name that matches your EHLO.** Receiving servers compare the
   two, and a mismatch costs you before the message is even read. Set the PTR
   record for your sending address with whoever gave you the IP, then pass the
   same name as `--ehlo`.

If the machine has several addresses and only one of them carries that PTR and
appears in your SPF record, pin it with `--source-ip`. On a machine with one
address, leave it alone.

### Configuration file

Passing everything on the command line gets old, and it puts your token in the
process list. The client reads a config file instead:

| Platform | Default path |
|---|---|
| Linux / BSD | `/etc/pushmails/client.cfg` |
| Windows | `%ProgramData%\PushMails\client.cfg` |
| macOS | `/etc/pushmails/client.cfg` |

That path is baked into the binary at build time, so a packaged install reads
the file the package puts down without anyone typing a path:

```bash
make build CONFIG_PATH=/opt/pushmails/client.cfg
```

Point at a different file with `--config /path/to/client.cfg` or
`PUSHMAILS_CONFIG`. A file you name explicitly must exist — if it cannot be
read the client stops rather than run with settings you did not intend. The
built-in default is allowed to be missing, so a setup driven purely by flags or
environment variables keeps working.

The format is `KEY=value`, one per line, with `#` comments — the same keys as
the environment variables below. See `packaging/client.cfg.example`:

```bash
# /etc/pushmails/client.cfg
PUSHMAILS_API=https://api.pushmails.net
PUSHMAILS_TOKEN=pm_live_xxxxxxxxxxxx

PUSHMAILS_MODE=direct
EHLO_NAME=mail.yourexample.com
BOUNCE_DOMAIN=bounce.yourexample.com
```

The file holds your token, so restrict it: `chmod 600 /etc/pushmails/client.cfg`
(on Windows, remove inherited permissions and grant only the service account).

**Precedence: flag > environment variable > config file > built-in default.**
The file only fills in what nothing else supplied, so a systemd unit or a
container that passes `SMTP_HOST` never loses to a file someone left on the
server. The startup log line reports which file was read.

### All options

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--config` | `PUSHMAILS_CONFIG` | build-time path | Config file to read |
| `--api` | `PUSHMAILS_API` | — | Central API address (required) |
| `--token` | `PUSHMAILS_TOKEN` | — | Agent token issued by the panel (required) |
| `--mode` | `PUSHMAILS_MODE` | `direct` | `direct` or `relay` |
| `--ehlo` | `EHLO_NAME` | hostname | Name given in EHLO; should match your PTR (direct) |
| `--source-ip` | `SOURCE_IP` | — | Address outgoing connections bind to (direct) |
| `--direct-timeout` | `DIRECT_TIMEOUT` | `60s` | Budget for one delivery, DNS included (direct) |
| `--smtp-host` | `SMTP_HOST` | `localhost` | SMTP server address (relay) |
| `--smtp-port` | `SMTP_PORT` | `25` | SMTP port (relay) |
| `--smtp-user` | `SMTP_USERNAME` | — | Authentication username (relay) |
| `--smtp-pass` | `SMTP_PASSWORD` | — | Authentication password (relay) |
| `--smtp-tls` | `SMTP_TLS` | `false` | Implicit TLS, usually port 465 (relay) |
| `--smtp-skip-verify` | `SMTP_SKIP_VERIFY` | `false` | Skip TLS certificate verification, local testing only (relay) |
| `--smtp-timeout` | `SMTP_TIMEOUT` | `30s` | SMTP operation timeout (relay) |
| `--bounce-domain` | `BOUNCE_DOMAIN` | — | Domain the envelope sender is built on (required) |
| `--concurrency` | `CONCURRENCY` | `4` | Number of concurrent sends |
| `--batch-size` | `BATCH_SIZE` | `50` | Jobs claimed per round |
| `--log-level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `--dry-run` | `DRY_RUN` | `false` | Exercise the protocol without sending email |

A flag overrides the matching environment variable.

### Trying it out for the first time

```bash
pushmails-client --api ... --token ... --dry-run --log-level debug
```

With `--dry-run` no email is sent; the connection, the token and the job-claiming
flow are exercised. Claimed jobs are handed straight back to the queue, so
nothing is marked as delivered and no campaign is consumed by a test run.

## DKIM signing

Every message is signed, and there is nothing to configure.

The keys come from the central system, one per verified sending domain, and are
held in memory only — nothing is written to disk. They are fetched for the
domains in the batch at hand, so a domain that is not sending today never has
its private key on the wire.

This is not a convenience. When you verify a domain in the panel it publishes a
DKIM public key in your DNS, and the matching private key is the one the whole
service signs that domain with. A key generated on this machine would not match
what your DNS advertises, so receivers would check the signature against the
wrong public key and report `dkim=fail` — while the panel still showed the
domain as verified.

Your own server signs with a selector of its own, separate from the one the
central pool uses for the same domain. That way a key on your machine can never
sign mail leaving ours, and it can be revoked on its own. The panel lists the
extra record once you add a server, and **no work is handed out until it is
published** — the check code is `self_dkim_unpublished`.

**Signing is mandatory.** A message whose domain has no key yet is handed back
to the queue rather than sent unsigned, and goes out on its own once the domain
finishes verifying. Unsigned mail from a domain that advertises a DKIM key
fails authentication outright, and with the envelope on your bounce domain
there is no SPF alignment left to carry DMARC either: the message would be
rejected, not merely filtered.

## Bounce handling

`--bounce-domain` is required, and it is what makes a bounce useful.

The envelope sender is built per message as
`bounce+<message id>@<your bounce domain>`. A rejection that arrives hours after
the receiving server accepted the message carries nothing else that identifies
it; without the id in the address there is no way to know which subscriber to
suppress, the dead addresses stay on your list, and the reputation they cost is
yours.

Use the `bounce` subdomain of the domain you send from. The panel lists the MX
record to add alongside your SPF, DKIM and DMARC rows, and verifies it the same
way — so `--bounce-domain bounce.yourexample.com` goes with:

```
bounce.yourexample.com.  MX  10  <the target the panel shows>
```

It has to be a domain of yours, not one of ours. The envelope domain is what
receiving servers run SPF against, and the connection comes from your machine:
a domain that does not authorise this machine turns a passing SPF into a
failing one, and takes the aligned path to DMARC with it.

## Installing from a package

Debian/Ubuntu and RHEL/Fedora/Rocky/Alma packages install the binary to
`/usr/bin`, the unit to `/usr/lib/systemd/system`, the example config to
`/etc/pushmails/client.cfg` (mode 0640, group `pushmails`) and create the
`pushmails` system user:

```bash
sudo apt install ./pushmails-client_1.0.0_amd64.deb      # Debian, Ubuntu
sudo dnf install ./pushmails-client-1.0.0-1.x86_64.rpm   # RHEL, Fedora, Rocky, Alma

sudo editor /etc/pushmails/client.cfg          # api address, token, bounce domain
sudo systemctl enable --now pushmails-client
```

The service is not started on install: the shipped config has no token, so it
could only fail. Upgrades keep your edited config (on RPM systems the new
example lands next to it as `client.cfg.rpmnew`) and restart a running client.

To build the packages yourself on a Linux machine with `dpkg-deb` or
`rpmbuild`:

```bash
make deb ARCH=amd64        # dist/pushmails-client_<version>_amd64.deb
make rpm ARCH=arm64        # dist/pushmails-client-<version>-1.aarch64.rpm
make packages              # both formats, both architectures
```

## Running under systemd

A ready-to-use unit ships in `packaging/systemd/pushmails-client.service`,
hardened and commented. Copy it rather than writing your own:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin pushmails
sudo install -m 0755 pushmails-client /usr/local/bin/

sudo install -d -m 0755 /etc/pushmails
sudo install -m 0640 -o root -g pushmails \
  packaging/client.cfg.example /etc/pushmails/client.cfg
sudo editor /etc/pushmails/client.cfg          # api address and token

sudo install -m 0644 packaging/systemd/pushmails-client.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now pushmails-client
sudo journalctl -u pushmails-client -f
```

The unit expects the binary at `/usr/local/bin/pushmails-client` and runs it as
the `pushmails` user, so anything it reads has to be readable by that account.
Settings come from `/etc/pushmails/client.cfg` (see above); no `ExecStart`
arguments are needed. The unit reads it through the binary rather than
`EnvironmentFile=`, so the same file works whether the client is started by
systemd or by hand.

`packaging/systemd/README.md` covers the rest: verifying the first run, sending
the logs to syslog, and running under init systems other than systemd.

## Running on Windows

Put the binary and its config where the build expects them:

```powershell
mkdir "C:\Program Files\PushMails"
copy pushmails-client.exe "C:\Program Files\PushMails\"

mkdir "%ProgramData%\PushMails"
copy packaging\client.cfg.example "%ProgramData%\PushMails\client.cfg"
```

Edit `%ProgramData%\PushMails\client.cfg`, then check it runs in the
foreground first:

```powershell
"C:\Program Files\PushMails\pushmails-client.exe" --dry-run --log-level debug
```

To keep it running, register it as a service — the client is a plain
long-running console program, so any service wrapper works:

```powershell
sc.exe create PushMailsClient ^
  binPath= "C:\Program Files\PushMails\pushmails-client.exe" start= auto
sc.exe start PushMailsClient
```

The config file holds your token: restrict it to the account the service runs
as and remove inherited permissions.

```powershell
icacls "%ProgramData%\PushMails\client.cfg" /inheritance:r ^
  /grant:r "SYSTEM:(R)" "Administrators:(F)"
```

## How it works

```
  ┌──────────────┐                          ┌──────────────────┐
  │ PushMails    │  1. hello                │ pushmails-client │
  │ central API  │ ◄────────────────────────│ (your server)    │
  │              │  2. jobs/claim long-poll │                  │
  │              │ ◄────────────────────────│                  │
  │              │ ─── mail to send ───────►│                  │
  │              │                          │        │ SMTP    │
  │              │  3. jobs/ack             │        ▼         │
  │              │ ◄─── results ────────────│   recipients     │
  └──────────────┘                          └──────────────────┘
```

1. **hello** — Introduces itself and picks up its limits: batch size, lease
   duration and the sending addresses registered for this node, with the
   blocklist status measured for each. It repeats every 60 seconds as a
   heartbeat, so a client that is busy sending still shows up as online in the
   panel, and the limits and addresses stay current without a restart. Alongside
   it, **preflight** reports the local checks described under
   [Health checks](#health-checks) and receives the verdict.
2. **claim** — Asks for work. When the queue is empty the connection is held open
   for up to 25 seconds; work arrives the moment a campaign starts. That is
   normal, not a timeout. The batch is sized to the rate this client actually
   achieves, so it finishes inside the lease instead of being taken back.
3. **Sending** — Turns the emails into MIME and signs them with DKIM if
   configured. In direct mode the batch is grouped by recipient domain, each
   group gets as many connections as that provider tolerates, messages to one
   infrastructure are spaced out, and a domain that starts refusing us is
   backed off from. Connections are reused between messages either way.
4. **ack** — Reports the results as they arrive, in chunks, rather than waiting
   for the whole batch. Permanent failures (a 5xx from the recipient) are not
   retried; temporary ones (4xx, network) go back into the queue.

## Health checks

Before it claims any work — and at an interval the central system chooses — the
client checks whether this installation can actually deliver mail, and reports
what it finds:

| Check | What it means |
|---|---|
| **Mail relay** | Relay mode only: can this client reach the SMTP server it is configured with? In direct mode there is no relay, so nothing is reported and the check produces no finding. |
| **Outbound port 25** | Can this host open a connection on port 25? Many providers block it by default. Only runs when the central system supplies a target. Decisive in direct mode, since this host is what does the sending; informational when your relay is on another machine. |
| **Blocklists** | Is the address your mail leaves from listed on a DNS blocklist? The lookups run centrally, so your server makes no extra queries. |
| **Limits** | Is the account active, and is there sending allowance left? |
| **Verification** | Is a sender address registered for the account, is its domain verified, and is the DKIM selector you sign with actually published? |
| **SPF authorisation** | Is the address this client connects from authorised by the SPF record of the domains you send as? Checked centrally, once per domain and address. |

The verdict belongs to the central system: this client reports what it measured
and obeys the answer. When something serious is wrong — no registered sender
address, an unreachable relay, a blocklisted address — sending pauses and every
reason is logged with a code you can look up in the table below. Warnings (a
missing DKIM record, a single-blocklist listing) are logged but never stop
sending.

Two things are deliberate:

- **A paused client keeps checking in.** It stays online in the panel and asks
  again on schedule, so it resumes on its own the moment the problem is fixed.
  No restart needed.
- **If the central system is unreachable, sending continues.** An outage on
  their side must not become an outage in your mail. The client only pauses on
  an answer, never on the absence of one.

### Check codes

Every finding is logged with a `code=`: blockers on a `sending is blocked` line,
warnings on a `health check warning` line. The same code arrives as the reason
when the central system declines to hand out work.

| Code | Stops sending | What it means |
|---|---|---|
| `account_suspended` | yes | The account is suspended. Contact support. |
| `quota_exceeded` | yes | The plan's monthly allowance is used up. |
| `no_sender_address` | yes | No sender address is registered for the account. Add one in the panel. |
| `sender_domain_unverified` | yes | None of your sending domains is verified. Once at least one is, a domain still pending only warns. |
| `relay_unreachable` | yes | Relay mode only: this client cannot reach its SMTP server. Check `--smtp-host` / `--smtp-port`. Direct mode never reports it. |
| `ip_blacklisted` | sometimes | Your sending address is on a DNS blocklist. A single listing warns; enough of them stop sending. The panel names the zones and when they were last checked — request delisting there. |
| `port25_blocked` | sometimes | Outbound port 25 is filtered. A blocker in direct mode and when the relay runs on this machine — either way this host is the one sending. Only a warning when the relay is elsewhere. |
| `port25_probe_failed` | no | The probe could not run (a refusal, a DNS failure). Not evidence of filtering. |
| `ip_reputation_unknown` | no | The blocklist measurement has not landed yet. Never a blocker: an outage on our side must not stop your mail. |
| `ip_not_in_spf` | no | See the note below. |
| `node_rate_limited` | no | The node's hourly ceiling is reached; `claim` comes back empty until the hour turns. |
| `self_dkim_unpublished` | yes | Sending from your own server signs with its own DKIM selector, and that record is not in DNS yet. The panel shows the row to add; sending starts on its own at the next check. |
| `bounce_domain_unverified` | no | Bounces sent to your `--bounce-domain` are not reaching us, so permanently rejected addresses stay on the list. Check the bounce record on the domains screen and that this setting names the same domain. |
| `dkim_selector_unpublished` | no | The selector is not in DNS. Finish the domain's verification in the panel. |
| `dkim_domain_mismatch` | no | The signing domain is not one of the account's verified domains, so the signature will not align. |

**`ip_not_in_spf` deserves its own note.** Adding the PushMails include to your
SPF record authorises *our* servers. It says nothing about yours. When this
client sends from your own machine, that machine's address has to appear in the
record too, or receiving servers see mail from an address the domain never
vouched for — and the domain looks green in the panel while the mail lands in
spam. The fix is one mechanism, for example `ip4:203.0.113.10`, added to the
existing `v=spf1` record of each domain named in the warning. The panel shows
the address this client connects from, which is the value to add.

**Not every 5xx is a bounce.** The client records *where* in the SMTP
conversation a rejection arrived. A 5xx at `RCPT TO`/`DATA` is about the
recipient and is reported as a permanent bounce. A 5xx at `AUTH` or `MAIL FROM`
is about *you* — your relay refusing the envelope sender or the credentials —
and is reported as temporary, so a misconfigured relay cannot permanently
suppress thousands of healthy addresses.

**No work is lost.** If claimed jobs are not reported within the lease, the
central system puts them back in the queue. Whether the process crashes or the
server reboots, the emails still go out.

**Shutdown is graceful.** On `SIGTERM` the client finishes the batch in hand,
reports the results and exits. Jobs it never started are returned explicitly, so
they keep their full retry budget.

Template processing happens centrally: fields like `{{ first_name }}` are
already filled in before the job reaches the client.

## Troubleshooting

**`invalid_agent_token`** — The token is wrong or was deleted in the panel. Get
a new one.

**`collector_token`** — This token may only report feedback, not send. Issue a
sender token in the panel instead.

**`could not connect to the SMTP server`** — Relay mode: verify the server you
pointed at with `--smtp-host`. On a local relay, `telnet localhost 25` and check
that Postfix or Exim is running. Direct mode never produces this — it has no
relay to connect to.

**`AUTH LOGIN cannot be used on an unencrypted connection`** — Authentication
requires an encrypted connection. Add `--smtp-tls`, or use a port that supports
STARTTLS (587).

**A setting has no effect** — Check the `config=` field on the startup log
line: it names the file that was actually read, or `(none)`. Remember the order
— a flag or an environment variable beats the file, so an `SMTP_HOST` exported
in the unit file will override the one in `client.cfg`.

**Mail lands in spam** — Turn on DKIM signing, check your SPF record and make
sure your sending IP is not blacklisted. In direct mode also check that your
address has a PTR record and that `--ehlo` is the same name: receiving servers
compare them, and a mismatch is one of the cheapest reasons to be filtered.

**`the server refused the session (possible blocklisting)`** — Direct mode: a
receiving server turned us away before it ever heard the recipient. Nothing is
wrong with the address, so nothing is suppressed, but a run of these means your
sending IP or its PTR is the problem. The client asks the central system for a
fresh health check as soon as it sees a batch go this way.

**`sending is blocked` in the log** — A health check failed. The line carries a
`code=`; look it up in [Check codes](#check-codes). Sending resumes on its own at
the next check once the cause is gone.

**`health check warning` in the log** — Something is worth fixing but nothing is
stopped. Same table, same `code=`.

**`health check failed; continuing to send`** — The client could not reach the
central system's health endpoint. This is a warning, not an outage: sending
carries on.

**No jobs ever arrive** — Make sure the campaign is set to "your own server"
mode in the panel and assigned to this server. Run with `--log-level debug` to
see the queue state.

## Contributing

Issues and pull requests are welcome. Before submitting a change:

```bash
go test ./...
go vet ./...
gofmt -l .
```

Please do not add dependencies — building with zero of them is a deliberate
choice for this project.

## License

MIT. See `LICENSE` for details.

You are free to use, modify and distribute it; keeping the copyright notice is
all that is required.
