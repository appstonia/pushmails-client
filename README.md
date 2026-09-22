# pushmails-client

Send your PushMails campaigns **from your own server**.

It connects to the central PushMails system, pulls the emails waiting to be
sent, delivers them and reports the results. The mail leaves your
infrastructure, from your IP address — the sending reputation is yours.

By default it is the mail server: it looks up the recipient's MX and talks to
it directly, with nothing else in the path. If you would rather keep an
existing relay in the middle, it can do that too.

- **Delivers by itself** — no Postfix, no Exim, no smarthost required
- **Next to no dependencies** — the Go standard library, plus `golang.org/x/sys`
  for the Windows service
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
2. **A reverse DNS name that matches your EHLO.** Major mailbox providers reject
   mail from an address with no PTR record, or with one whose name does not
   resolve back to the same address. The central system checks this for the
   address the client connects from, and **no work is handed out until it
   passes** — the check code is `ptr_invalid`. Set the PTR record with whoever
   gave you the IP, make sure that name resolves back to the address, then
   pass the same name as `--ehlo`.

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
sign mail leaving ours, and it can be revoked on its own. The panel lists that
record for each domain you assign to the server, and **no work is handed out
until it is published** — the check code is `self_dkim_unpublished`.

### Which domains a server sends for

When you add a server in the panel you choose the domains it sends for, and
you can change the choice later from the servers screen. Nothing is picked for
you: every domain on the list is one more DNS record to publish, and a domain
you only ever send through PushMails should not need one.

The choice decides three things:

- **The records you are asked for.** The extra DKIM record appears on the
  domains screen only for the domains assigned to a server.
- **The keys this client receives.** It is given signing keys for its own
  domains and nothing else, so a compromised machine exposes only those.
- **What can be sent through it.** A campaign sending as a domain that is not
  assigned to the chosen server is refused when you start it, rather than
  queued for a server that could never sign it.

Every assigned domain has to be ready — verified, with its DKIM record
published — before the server sends at all. If one is not ready yet and you do
not want to wait, remove it from the server.

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

The Debian/Ubuntu and RHEL/Fedora/Rocky/Alma packages install the binary to
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

Packages are published on the
[releases page](https://github.com/appstonia/pushmails-client/releases) for
`amd64` and `arm64`. If you would rather not use one, build from source and
install the unit by hand — the next section covers that, and nothing about the
client requires a package.

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

On Windows the client runs as a real Windows service. There are two ways in:
the MSI, which installs the binary and registers the service for you, or a
build from source and `sc.exe`.

### With the installer

Download `pushmails-client-<version>-x64.msi` (or `-arm64.msi`) from the
[releases page](https://github.com/appstonia/pushmails-client/releases) and
run it, or install it unattended:

```powershell
msiexec /i pushmails-client-1.0.0-x64.msi /qn
```

It puts the binary in `C:\Program Files\PushMails`, the config in
`C:\ProgramData\PushMails\client.cfg` with the permissions it needs, and
registers the `pushmails-client` service — **on manual start, and not
running**. The config that ships with it has no token, so a service set to
start by itself could only fail. Fill the file in first:

```powershell
Start-Process notepad -ArgumentList "C:\ProgramData\PushMails\client.cfg" -Verb RunAs
```

Then turn it on:

```powershell
start sc.exe -ArgumentList "config pushmails-client start= auto" -Verb RunAs
start sc.exe -ArgumentList "start pushmails-client" -Verb RunAs
```

Upgrades keep your edited config and restart the service. Uninstalling removes
the service, the program files **and the config**, token included — keep a copy
first if you mean to reinstall.

### From source

```powershell
git clone https://github.com/appstonia/pushmails-client.git
cd pushmails-client
go build -o pushmails-client.exe .\cmd\pushmails-client
```

Go cross-compiles, so the machine you build on does not have to match the one
you run on. On Windows-on-ARM, `$env:GOARCH="amd64"; go build …` produces an
x64 binary — which Windows 11 on ARM also runs under emulation, so `amd64` is
the safe answer whenever you are unsure. Unset the variable afterwards
(`Remove-Item Env:GOARCH`); it stays set for the rest of the session.

Put the binary and its config where the service will look for them, from an
elevated PowerShell:

```powershell
New-Item -ItemType Directory -Force "$env:ProgramFiles\PushMails" | Out-Null
Copy-Item .\pushmails-client.exe "$env:ProgramFiles\PushMails\"

New-Item -ItemType Directory -Force "$env:ProgramData\PushMails" | Out-Null
Copy-Item .\packaging\client.cfg.example "$env:ProgramData\PushMails\client.cfg"
notepad "$env:ProgramData\PushMails\client.cfg"
```

`%ProgramData%\PushMails\client.cfg` is the path the binary looks at by
default, so no `--config` argument is needed once the file is there. Save it as
UTF-8; a BOM is tolerated, a file saved as UTF-16 is not readable.

The file holds your agent token and `%ProgramData%` is readable by every user
by default, so drop the inherited permissions:

```powershell
icacls "$env:ProgramData\PushMails\client.cfg" /inheritance:r `
  /grant:r "SYSTEM:(R)" "Administrators:(F)"
```

Check it in the foreground before registering anything:

```powershell
& "$env:ProgramFiles\PushMails\pushmails-client.exe" --dry-run --log-level debug
```

No mail is sent: the connection, the token and the job-claiming round trip are
exercised, and claimed jobs go straight back to the queue. Within a few seconds
a preflight line says whether sending is allowed and, if it is not, which check
failed (see [Check codes](#check-codes)). Stop it with Ctrl+C.

Then register the service:

```powershell
sc.exe create pushmails-client start= auto `
  binPath= "\"$env:ProgramFiles\PushMails\pushmails-client.exe\"" `
  DisplayName= "PushMails Client"
sc.exe description pushmails-client "Sends PushMails campaigns from this server."
sc.exe failure pushmails-client reset= 86400 actions= restart/60000/restart/60000/restart/60000
sc.exe start pushmails-client
```

The binary detects that the service manager started it and reports its status
accordingly; no wrapper such as WinSW or NSSM is needed. The same binary still
runs in the foreground when you start it yourself.

### Logs

Started as a service, the client has no console, so it writes to
`C:\ProgramData\PushMails\client.log` — the whole log, not just errors. Set
`PUSHMAILS_LOG_FILE` to put it somewhere else. The file rolls over at 10 MB and
one previous generation is kept as `client.log.1`.

```powershell
Get-Content "$env:ProgramData\PushMails\client.log" -Wait -Tail 20
```

Started from a console it writes to stdout as it does everywhere else.

A stop is graceful: the client finishes the batch in hand, reports the results
and returns anything it never started. Nothing is lost even if it is killed —
a batch that is never acknowledged goes back into the queue on the central
system when its lease expires.

### Port 25 on a Windows host

In the default `direct` mode the client connects to each recipient's mail
server on port 25. Two things commonly stand in the way:

- **The provider.** Azure, and most Windows VPS providers, block outbound 25 by
  default. Nothing on the machine works around it — it is either lifted on
  request or it is not. The client reports it as `port25_blocked`.
- **Windows Firewall.** Outbound connections are allowed by default, so nothing
  is needed unless that policy was changed. If it was:

  ```powershell
  New-NetFirewallRule -DisplayName "PushMails client (SMTP out)" `
    -Direction Outbound -Protocol TCP -RemotePort 25 -Action Allow `
    -Program "$env:ProgramFiles\PushMails\pushmails-client.exe"
  ```

No inbound rule is ever needed; the client only makes outbound connections.

Direct mode also wants a PTR record for the address the machine sends from, and
that address in the SPF record of every domain it sends for. Both show up as
check codes when they are missing. If you can get neither port 25 nor a PTR
record, use `relay` mode and hand the mail to an SMTP server that has them.

### Upgrading and uninstalling

With the MSI, run the new one — it stops the service, replaces the binary,
starts it again and leaves your config alone. To remove it, use **Apps &
features** or `msiexec /x`; that takes the config with it, so copy the file
first if you will want the token again.

With a build from source:

```powershell
sc.exe stop pushmails-client
Copy-Item .\pushmails-client.exe "$env:ProgramFiles\PushMails\" -Force
sc.exe start pushmails-client
```

Removing that one is `sc.exe delete pushmails-client` plus the two folders.

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
| **Verification** | Is a sender address registered for the account, is a domain assigned to this server, and is every assigned domain verified with its DKIM record published? |
| **Reverse DNS** | Does the address this client connects from have a PTR record that resolves back to it? Checked centrally. Decisive when this host does the sending (direct mode, or a relay on the same machine); not asked when your relay is elsewhere. |
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
| `no_assigned_domain` | yes | No domain is assigned to this server. Choose its domains on the servers screen in the panel. |
| `assigned_domain_unverified` | yes | A domain assigned to this server is not verified yet. Finish its DNS records, or remove it from the server. |
| `relay_unreachable` | yes | Relay mode only: this client cannot reach its SMTP server. Check `--smtp-host` / `--smtp-port`. Direct mode never reports it. |
| `ip_blacklisted` | sometimes | Your sending address is on a DNS blocklist. A single listing warns; enough of them stop sending. The panel names the zones and when they were last checked — request delisting there. |
| `port25_blocked` | sometimes | Outbound port 25 is filtered. A blocker in direct mode and when the relay runs on this machine — either way this host is the one sending. Only a warning when the relay is elsewhere. |
| `port25_probe_failed` | no | The probe could not run (a refusal, a DNS failure). Not evidence of filtering. |
| `ip_reputation_unknown` | no | The blocklist measurement has not landed yet. Never a blocker: an outage on our side must not stop your mail. |
| `ip_not_in_spf` | no | See the note below. |
| `node_rate_limited` | no | The node's hourly ceiling is reached; `claim` comes back empty until the hour turns. |
| `self_dkim_unpublished` | yes | A domain assigned to this server has no published record for its own DKIM selector. The panel shows the row to add; sending starts on its own at the next check. |
| `ptr_invalid` | yes | The address this client connects from has no PTR record (`variant=missing`), has one that does not resolve back to it (`mismatch`), or has not been checked yet (`pending`, clears within minutes). Only when this host does the sending. A lookup failure on our side never produces it. |
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
