# AutoHeal agent

`autoheal-agent` is a small, single-binary service that watches one server for
[AutoHeal](https://github.com/alimrun/autoheal-agent). It reads the host's CPU,
memory and disk use, runs the HTTP and TCP checks you configure, and sends the
results to your AutoHeal environment, where a failing check opens an incident.

It never runs commands, never starts a process, and takes no instructions or
configuration from AutoHeal. See [Security model](#security-model).

```
autoheal-agent enroll   [--config PATH] [--token-stdin] [--force]
autoheal-agent run      [--config PATH]
autoheal-agent validate [--config PATH]
autoheal-agent version
```

> **Releases are unsigned previews for now.** The release pipeline signs
> `SHA256SUMS` with [minisign](https://jedisct1.github.io/minisign/), but the
> release key does not exist yet. Until the product owner creates it with
> `minisign -G` and commits only its public half as `minisign.pub` at the root
> of this repository, releases carry checksums but no signature you can check.
> Once `minisign.pub` is here, verify every download as shown below.

## 1. Requirements

- Linux (`amd64` or `arm64`) with systemd, or macOS (Intel or Apple silicon)
  with launchd.
- Outbound HTTPS from the server to your AutoHeal address.
- A reasonably accurate clock (NTP). AutoHeal refuses events dated more than
  5 minutes in the future.
- An enrollment token, created on the environment's Agents page in the
  AutoHeal dashboard. It is shown once.
- `shasum` (or `sha256sum`) to check the download, and `minisign` to check the
  signature once releases are signed.

No other software is needed: the binary is statically linked.

## 2. Download and verify

From the release, download the archive for your platform, `SHA256SUMS` and,
once releases are signed, `SHA256SUMS.minisig`:

```
autoheal-agent_<version>_linux_amd64.tar.gz
autoheal-agent_<version>_linux_arm64.tar.gz
autoheal-agent_<version>_darwin_amd64.tar.gz
autoheal-agent_<version>_darwin_arm64.tar.gz
```

Check the archive against the checksums:

```bash
shasum -a 256 -c SHA256SUMS --ignore-missing
```

Check that the checksums were published by AutoHeal (signed releases only).
`<key>` is the key line from `minisign.pub` in this repository; get it from the
repository, not from the release page:

```bash
minisign -Vm SHA256SUMS -P <key>
```

Both must succeed. If either fails, do not install the archive.

## 3. Install

Unpack the archive and install the binary (Linux shown; on macOS use
`darwin_arm64` or `darwin_amd64`):

```bash
tar -xzf autoheal-agent_<version>_linux_amd64.tar.gz
cd autoheal-agent_<version>_linux_amd64
sudo install -m 0755 autoheal-agent /usr/local/bin/autoheal-agent
autoheal-agent version
```

The agent runs as its own unprivileged user, `autoheal-agent`, which owns the
state folder. The state folder holds the credential and the spool of unsent
events, and must be readable only by that user: the agent refuses to start if
the folder or the credential file is open to group or other users.

| | Linux | macOS |
|---|---|---|
| Binary | `/usr/local/bin/autoheal-agent` | `/usr/local/bin/autoheal-agent` |
| Config | `/etc/autoheal/agent.yaml` | `/usr/local/etc/autoheal/agent.yaml` |
| State folder (`0700`) | `/var/lib/autoheal-agent` | `/usr/local/var/autoheal-agent` |
| Credential (`0600`) | `/var/lib/autoheal-agent/credential.json` | `/usr/local/var/autoheal-agent/credential.json` |
| Logs | the journal (`journalctl -u autoheal-agent`) | `/usr/local/var/log/autoheal-agent.log` |

**Linux:**

```bash
sudo useradd --system --no-create-home --home-dir /var/lib/autoheal-agent --shell /usr/sbin/nologin autoheal-agent
sudo install -d -m 0755 /etc/autoheal
sudo install -d -m 0700 -o autoheal-agent -g autoheal-agent /var/lib/autoheal-agent
```

**macOS** (pick a free ID below 500 for the user and group; `dscl . -list /Users UniqueID` shows the ones in use):

```bash
sudo dscl . -create /Groups/autoheal-agent PrimaryGroupID 480
sudo dscl . -create /Users/autoheal-agent UniqueID 480
sudo dscl . -create /Users/autoheal-agent PrimaryGroupID 480
sudo dscl . -create /Users/autoheal-agent UserShell /usr/bin/false
sudo dscl . -create /Users/autoheal-agent NFSHomeDirectory /var/empty
sudo dscl . -create /Users/autoheal-agent IsHidden 1
sudo install -d -m 0755 /usr/local/etc/autoheal /usr/local/var/log
sudo install -d -m 0700 -o autoheal-agent -g autoheal-agent /usr/local/var/autoheal-agent
sudo touch /usr/local/var/log/autoheal-agent.log
sudo chown autoheal-agent:autoheal-agent /usr/local/var/log/autoheal-agent.log
sudo chmod 0600 /usr/local/var/log/autoheal-agent.log
```

## 4. Configure

Copy the example and edit it. Every setting is described in
[`packaging/agent.example.yaml`](packaging/agent.example.yaml): the agent's
name and check prefix, the state folder, an optional extra CA file, the log
level, the spool limits, the host thresholds, and up to 50 HTTP and TCP checks.

```bash
sudo install -m 0644 packaging/agent.example.yaml /etc/autoheal/agent.yaml   # macOS: /usr/local/etc/autoheal/agent.yaml
sudoedit /etc/autoheal/agent.yaml
autoheal-agent validate                                                       # or: --config PATH
```

`validate` prints what is wrong, with line numbers, and changes nothing. The
config never holds the AutoHeal address or a secret; those come from
enrollment.

## 5. Enroll

Enrollment exchanges the one-time token for the agent's own credential. Run it
once, as the service user, with two variables from the dashboard's token page:

- `AUTOHEAL_URL`: the API address the dashboard shows, ending in `/api/v1`. It
  must be `https://`, or `http://` to `localhost`, `127.0.0.1` or `::1` only.
- `AUTOHEAL_ENROLLMENT_TOKEN`: the token.

```bash
sudo -u autoheal-agent env \
  AUTOHEAL_URL='https://autoheal.example.com/api/v1' \
  AUTOHEAL_ENROLLMENT_TOKEN='<token>' \
  autoheal-agent enroll
```

Or keep the token out of the environment and pipe it in:

```bash
printf '%s\n' '<token>' | sudo -u autoheal-agent env AUTOHEAL_URL='https://autoheal.example.com/api/v1' autoheal-agent enroll --token-stdin
```

Never pass the token as a command-line argument, where other users can see it
in the process list; `enroll` refuses one. On macOS add
`--config /usr/local/etc/autoheal/agent.yaml` only if you keep the config
elsewhere.

`enroll` writes `credential.json` into the state folder with mode `0600` and
prints the agent, project and environment it joined. If the agent is already
enrolled it stops; run `autoheal-agent enroll --force` with a new token to
replace the credential (for example after the old one was revoked). A running
agent notices the new credential within 5 minutes without a restart.

## 6. Run as a service

**Linux (systemd):**

```bash
sudo install -m 0644 packaging/autoheal-agent.service /etc/systemd/system/autoheal-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now autoheal-agent
systemctl status autoheal-agent
journalctl -u autoheal-agent -f
```

The unit runs `autoheal-agent run` as `autoheal-agent`, restarts it 10 seconds
after a failure, and sandboxes it (`NoNewPrivileges`, `ProtectSystem=strict`,
`ProtectHome`, `PrivateTmp`, no capabilities, IP and Unix sockets only).

**macOS (launchd):**

```bash
sudo install -m 0644 -o root -g wheel packaging/com.autoheal.agent.plist /Library/LaunchDaemons/com.autoheal.agent.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.autoheal.agent.plist
sudo launchctl print system/com.autoheal.agent | head -20
tail -f /usr/local/var/log/autoheal-agent.log
```

launchd starts it at boot and restarts it after a non-zero exit.

To run it in the foreground instead, for a first test: `sudo -u autoheal-agent autoheal-agent run`.
It logs JSON lines to standard error and stops cleanly on `Ctrl-C` or `SIGTERM`,
saving events it has not sent yet.

## 7. Check the status on the dashboard

Open the environment's **Agents** page. Within a minute of starting, the agent
shows as online with its hostname, version and platform, and its last
heartbeat. Its checks appear as `<host_prefix>:<check name>` (for example
`web-01:api`, `web-01:host.cpu`). A check that goes down opens an incident; it
resolves when the check is up again. An **Update required** marker means this
version is older than the minimum AutoHeal supports.

## 8. Upgrade

Upgrade in place; the config, the credential and the spool stay where they are.

```bash
# Linux
sudo systemctl stop autoheal-agent
sudo install -m 0755 autoheal-agent /usr/local/bin/autoheal-agent
sudo systemctl start autoheal-agent

# macOS
sudo launchctl bootout system/com.autoheal.agent
sudo install -m 0755 autoheal-agent /usr/local/bin/autoheal-agent
sudo launchctl bootstrap system /Library/LaunchDaemons/com.autoheal.agent.plist
```

Verify the new download first (section 2). Events spooled while the agent was
stopped are sent when it starts, and the dashboard shows the new version on the
next heartbeat.

## 9. Uninstall

First **revoke the agent in the dashboard** (on the environment's Agents page), so
its credential stops working even if a copy of it survives. Then:

```bash
# Linux
sudo systemctl disable --now autoheal-agent
sudo rm /etc/systemd/system/autoheal-agent.service
sudo systemctl daemon-reload
sudo rm -r /var/lib/autoheal-agent /etc/autoheal
sudo rm /usr/local/bin/autoheal-agent
sudo userdel autoheal-agent

# macOS
sudo launchctl bootout system/com.autoheal.agent
sudo rm /Library/LaunchDaemons/com.autoheal.agent.plist
sudo rm -r /usr/local/var/autoheal-agent /usr/local/etc/autoheal
sudo rm /usr/local/var/log/autoheal-agent.log /usr/local/bin/autoheal-agent
sudo dscl . -delete /Users/autoheal-agent
sudo dscl . -delete /Groups/autoheal-agent
```

## 10. Troubleshooting

Logs are JSON lines; the `msg` field is quoted below.

| Log line | What it means | What to do |
|---|---|---|
| `AutoHeal refused this agent's credential; nothing will be sent until it is enrolled again with autoheal-agent enroll --force` | The credential was revoked, expired, or lacks a scope. Events keep spooling within the limits. | Create a new token on the dashboard and run `autoheal-agent enroll --force` as the service user. The running agent picks it up within 5 minutes. |
| `the agent was enrolled with a different API address; restart it to use the new one` | `enroll --force` pointed the agent at another AutoHeal address. | Restart the service. |
| `spool full: dropping the oldest events` | AutoHeal has been unreachable long enough to fill `spool.max_events` or `spool.max_mb`. | Fix the connection (see "sending telemetry failed"). Raise the spool limits if outages this long are expected. |
| `dropping events too old for AutoHeal to accept` | Spooled events passed 23 hours of age; AutoHeal accepts only the last 24 hours. | Nothing to do; the outage was longer than that. |
| `AutoHeal refused a batch as invalid; dropping it` | AutoHeal rejected the events (the `fields` show why). Most often the clock is far ahead. | Fix the clock (below). If it is right, report the fields to AutoHeal support. |
| `this host's clock differs from AutoHeal's; events dated more than 5 minutes ahead are refused` | The local time and AutoHeal's differ by more than 2 minutes. | Enable NTP (`timedatectl set-ntp true`, or System Settings → Date & Time on macOS). |
| `this agent is older than the minimum version AutoHeal supports; upgrade it` | AutoHeal still accepts this version but asks for an upgrade. Logged at most hourly. | Upgrade (section 8). |
| `sending telemetry failed; events stay in the spool and are retried with backoff` / `heartbeat failed; retrying with backoff` | AutoHeal could not be reached, timed out, or asked the agent to slow down. Retries back off up to 5 minutes. | Check DNS, the firewall and proxy (`HTTPS_PROXY` is honoured), and `ca_file` for an internal CA. `telemetry delivered again` / `heartbeat delivered again` confirm recovery. |
| `events are arriving faster than they can be saved; dropping results` | The disk holding the state folder is slow or full. | Free disk space on that volume. |
| `… can be read by other users (mode 0644); run: chmod 600 …` | The state folder or `credential.json` allows group or other access, so the agent refuses it. | Run the `chmod` it prints (`0700` for the folder, `0600` for the file), and keep both owned by `autoheal-agent`. |

## 11. Security model

**What it sends**, only to the AutoHeal API address that issued its credential:

- at enrollment and on each heartbeat: the agent's name, hostname, version,
  operating system and architecture;
- check results: the check name and kind, up / degraded / down, the target
  (a sanitized URL, a host and port, or a disk path), the measured percentage
  or latency, a short error detail, and the agent version. URLs in results and logs
  are reduced to scheme, host, port and path; user information, query strings
  and fragments are dropped.

It sends no file contents, process lists, environment variables or logs.

**What it never does:**

- It never runs commands or starts a process. Nothing in the binary can: a
  test builds every release target and fails if the linker kept any function
  that starts one (`os.StartProcess`, `os/exec`, `syscall.forkExec`,
  `syscall.Exec`), and a source scan forbids them and any way to skip TLS
  certificate checks.
- It takes no configuration or instructions from AutoHeal. What it checks is
  decided only by the local config file; answers from AutoHeal set only the
  heartbeat interval, credential rotation and the minimum version notice.
- It does not follow redirects and always verifies TLS certificates (TLS 1.2 or
  later; your system's CAs plus an optional `ca_file`).
- It never logs the enrollment token or the credential, and refuses a token
  passed as a command-line argument.

**Where the credential lives:** `credential.json` in the state folder
(`/var/lib/autoheal-agent` on Linux, `/usr/local/var/autoheal-agent` on
macOS), mode `0600` in a `0700` folder owned by `autoheal-agent`. It is a
bearer secret, stored in plain text as any client's own secret must be; the
file permissions protect it, and the agent refuses to use it if they are
loosened. It is only ever sent to the API address saved next to it at
enrollment, never to an address from the config file. It is rotated
automatically before it expires. Revoke it on the dashboard if the server may
be compromised.

## Building from source

Requires Go (see `go.mod`).

```bash
make build      # ./autoheal-agent, version 0.0.0-dev unless HEAD is tagged
make check      # gofmt, go vet, go test -race (with the no-process build check), govulncheck if installed
make release V=0.1.0
```

`scripts/release.sh <version>` needs a clean tree and the tag `v<version>` on
`HEAD`. It builds the four targets with `CGO_ENABLED=0 -trimpath`, packs
reproducible archives into `dist/`, writes `dist/SHA256SUMS`, and signs it with
the minisign secret key file named by `MINISIGN_SECRET_KEY`. Without that
variable it stops after the checksums, signs nothing, and exits with status 3.
