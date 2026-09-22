# Running under systemd

`pushmails-client.service` in this directory is a ready-to-use unit. It expects
the binary at `/usr/local/bin/pushmails-client` and its settings at
`/etc/pushmails/client.cfg` (see `../client.cfg.example`).

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin pushmails

sudo install -m 0755 pushmails-client /usr/local/bin/
sudo install -d -m 0755 /etc/pushmails

# The file holds your agent token, so keep it readable only by the service.
sudo install -m 0640 -o root -g pushmails packaging/client.cfg.example \
     /etc/pushmails/client.cfg
sudo editor /etc/pushmails/client.cfg

sudo install -m 0644 packaging/systemd/pushmails-client.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now pushmails-client
```

There is no key file to install. Signing keys come from the central system,
one per verified sending domain, and are held in memory for as long as the
process runs.

## Checking that it works

```bash
systemctl status pushmails-client
journalctl -u pushmails-client -f
```

A healthy client logs a preflight result shortly after start. If it reports
that it cannot send, the reason is in that line: the server decides whether
sending is allowed and says why, so you do not have to guess between a blocked
port, a failed relay connection, and an unverified domain.

## Sending the logs to syslog

The unit writes to the journal, which is where systemd puts service output by
default. `journalctl -u pushmails-client` is usually all you need.

If you collect logs with syslog instead, turn on forwarding for the machine:

```bash
sudo install -d -m 0755 /etc/systemd/journald.conf.d
printf '[Journal]\nForwardToSyslog=yes\n' | \
    sudo tee /etc/systemd/journald.conf.d/10-forward-syslog.conf
sudo systemctl restart systemd-journald
```

Lines then arrive at your syslog daemon tagged `pushmails-client`. To give them
their own file with rsyslog, drop this in `/etc/rsyslog.d/49-pushmails.conf`:

```
if $programname == "pushmails-client" then {
    action(type="omfile" file="/var/log/pushmails-client.log")
    stop
}
```

The `stop` keeps the same lines out of `/var/log/syslog`; remove it if you want
them in both. Output is plain text, one line per event, so it greps cleanly and
needs no parser.

Do not set `StandardOutput=syslog` in the unit. That option was deprecated in
systemd 246 and is mapped back to the journal, so it looks like it does
something and does not — the forwarding switch above is what actually moves the
lines.

## Other init systems

The client is a plain long-running console program that logs to stdout and
stops on SIGTERM, so any supervisor works — OpenRC, runit, s6. The only
requirements are that it can read `/etc/pushmails/client.cfg` and reach both
your relay and the API. On Windows it registers with the service manager
itself; see "Running on Windows" in the main README.
