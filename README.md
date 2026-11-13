# AutoHeal agent

`autoheal-agent` is a small, single-binary service that watches one server for
[AutoHeal](https://github.com/alimrun/autoheal-agent). It reads the host's CPU,
memory and disk use, runs the HTTP and TCP checks you configure, and sends the
results to your AutoHeal environment, where a failing check opens an incident.

It never runs commands, never starts a process, and takes no configuration
from AutoHeal. Recovery is off by default. If you turn it on, AutoHeal can ask
the agent for two actions on targets you declared in its config: run a check
again, or call a cache hook on the same host. See [Recovery](#8-recovery-optional)
and the [Security model](#12-security-model).

```
autoheal-agent enroll   [--config PATH] [--token-stdin] [--force]
autoheal-agent run      [--config PATH]
autoheal-agent validate [--config PATH]
autoheal-agent version
```

> **Releases are unsigned previews for now.** The release pipeline signs
> `SHA256SUMS` with [minisign](https://jedisct1.github.io/minisign/), but the
> release key does not exist yet. Until the product owner creates it and
> commits only its public half as `minisign.pub` at the root of this
> repository (see [Releasing](#releasing)), releases carry checksums but no
> signature you can check. Once `minisign.pub` is here, verify every download
> as shown below; the install script then checks the signature for you.

## Quick install

On the server, as a user who can `sudo`:

```bash
curl -fsSL https://github.com/alimrun/autoheal-agent/releases/latest/download/install.sh | sudo sh
```

The environment's **Agents** page shows the same command with `--url` already
filled in. The script asks on the terminal for anything you did not pass:

1. the API address, ending in `/api/v1`;
2. the agent's name on the dashboard (the host name by default);
3. the system user the service runs as (`autoheal-agent` by default);
4. the enrollment token from the dashboard. It is read with echo off, never
   from the command line or the environment, never printed, and never written
   to your shell history.

Before it changes anything it detects the distribution and the architecture
(`amd64` or `arm64`), checks for curl or wget, tar, a SHA-256 tool, useradd and
a running systemd (and prints the `apt-get`/`dnf`/`zypper`/`apk` command for
anything missing), downloads the release and refuses it on a checksum mismatch
(and, once releases are signed, on a bad minisign signature), then shows what
it will do and asks `Install? [Y/n]`. It then:

1. creates the service user and the folders with the modes in section 3,
   installs the binary, and writes a starter config that reports CPU, memory
   and disk use (an existing config is kept);
2. enrolls as the service user with `autoheal-agent enroll --token-stdin`;
3. installs and starts the systemd unit (or the launchd daemon on macOS);
4. prints a summary and `systemctl status`.

Running it again upgrades the binary and restarts the service, keeping the
config and the credential.

### Several projects on one server

One agent reports to one project environment. When a server runs apps for
several projects, run the same command again with a token from the other
environment: the script sees the existing agent and offers to add another one
beside it. Each agent gets a short instance name, its own config, credential
and service, and they share the binary and the service user:

| Instance | Config | State | Service |
|---|---|---|---|
| first (`default`) | `/etc/autoheal/agent.yaml` | `/var/lib/autoheal-agent` | `autoheal-agent` |
| `billing-prod` | `/etc/autoheal/billing-prod.yaml` | `/var/lib/autoheal-agent-billing-prod` | `autoheal-agent-billing-prod` |

On macOS the paths are under `/usr/local` and the daemon is
`com.autoheal.agent.<instance>`. Running the script upgrades the binary for all
of them and restarts them.

### Automation

Without a terminal (Ansible, cloud-init, CI), pass every input as an option,
`--yes`, and the token on standard input from a file only root can read:

```bash
sudo sh install.sh --url https://autoheal.example.com/api/v1 --name web-01 --yes --token-stdin < /root/autoheal-token
```

The script refuses to run without a terminal unless `--url`, `--token-stdin`
and `--yes` are given. The options can also come from the environment
(`sudo AUTOHEAL_URL=… AUTOHEAL_YES=1 sh install.sh --token-stdin < file`); the
token never can.

| Option | Environment | |
|---|---|---|
| `--url URL` | `AUTOHEAL_URL` | The API address, ending in `/api/v1`. |
| `--name NAME` | `AUTOHEAL_AGENT_NAME` | The agent's name on the dashboard, for a new config. Defaults to the host name. |
| `--user USER` | `AUTOHEAL_SERVICE_USER` | The service user. Defaults to `autoheal-agent`; all agents on a server share it, and root is refused. |
| `--instance NAME` | `AUTOHEAL_INSTANCE` | Install or upgrade a further agent for another project. |
| `-y`, `--yes` | `AUTOHEAL_YES=1` | Do not ask to confirm, and take the defaults for the name and the user. |
| `--token-stdin` | | Read the token from standard input. Only when the script is a downloaded file, not piped from curl. |
| `--reenroll` | | Ask for a new token and replace the credential (`enroll --force`). |
| `--version X.Y.Z` | | Install that release instead of the latest. |
| `--no-start` | | Install and enroll, but leave the service stopped. |
| `--download-base URL` | | Fetch the release files from a mirror. |

Add your HTTP and TCP checks to the config afterwards (section 4) and restart
the service. To read the script first, download it, read it, then run
`sudo sh install.sh`. The sections below are the same steps by hand.

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
level, the spool limits, the host thresholds, up to 50 HTTP and TCP checks, and
the optional recovery section (section 8).

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

## 8. Recovery (optional)

From version 0.2.0 the agent can act on recovery tasks. It is **off unless the
config turns it on**, and an agent with recovery off never asks AutoHeal for
tasks.

With recovery on, the agent asks AutoHeal for a task every 15 seconds and
holds one at a time. It can do exactly two things, and only to targets named in
its own config:

| Action | What the agent does |
|---|---|
| `recheck_health` | Runs one of its configured checks once, now, and sends the result as ordinary telemetry. |
| `clear_cache` | Sends one HTTP `POST` to a cache hook you declared, on the same host. |

Anything else is refused. The agent does not take checks, hook addresses or
commands from AutoHeal, and it still cannot start a process.

### Key setup

AutoHeal signs every task with an Ed25519 key, and the agent acts only on
tasks signed by a key listed in its config.

1. On the AutoHeal server, the administrator generates the signing key once:
   `php artisan recovery:generate-signing-key`. It prints a **key ID** and a
   **public key** (32 bytes, base64). The secret key stays on the server.
2. Put both in the agent's config under `recovery.platform_keys`. The public
   key is not a secret, but get it from your administrator, not from a
   message you cannot trace.
3. To rotate the key, list the new key next to the old one on every agent,
   switch the server to the new key, then remove the old one.

### Turn it on

Add this to the config (the example file has it commented out), then run
`autoheal-agent validate` and restart the service:

```yaml
recovery:
  enabled: true
  platform_keys:
    - id: k1
      public_key: "<base64 Ed25519 public key>"
  actions:
    recheck_health:
      checks: [api]                 # names from `checks:`, or host.cpu, host.memory, host.disk.<name>
    clear_cache:
      hooks:
        - name: views               # the cache name a task will ask for
          url: "http://127.0.0.1:8080/internal/cache/clear"
          token_file: /etc/autoheal/hooks/views.token   # optional
          timeout: 30s              # default 30s, at most 120s
```

`enabled: true` needs at least one key and at least one action with a target.
An action with no targets is off. `validate` refuses, with the line number: a
key that is not 32 bytes of base64, a check name the agent does not run, a hook
address that is not on this host, and a token file other users can read.

The agent's heartbeat tells AutoHeal which of the two actions are on, never
the check or hook names. Recovery also has to be turned on in AutoHeal's own
recovery policy, and the agent's credential needs the `recovery` scope: a
credential issued before 0.2.0 is rotated once, automatically, to get it.

### The cache hook contract

A hook is an HTTP endpoint **you** provide in your application; what it clears
is up to you. The agent's side of the contract:

- **Loopback only.** The URL's host must be `localhost`, an address in
  `127.0.0.0/8` or `::1`. The agent also checks the address it actually
  connects to, so a name that resolves elsewhere is refused. It uses no proxy.
- **One `POST`** per task, with `Content-Type: application/json` and the body
  `{"task_id": "<task ID>"}`. Nothing from AutoHeal other than that ID reaches
  the hook.
- **Optional bearer token.** With `token_file` set, the file's content
  (trimmed) is sent as `Authorization: Bearer <token>`. The file must be
  readable only by the agent's user:

  ```bash
  sudo install -d -m 0750 -o root -g autoheal-agent /etc/autoheal/hooks
  sudo install -m 0600 -o autoheal-agent -g autoheal-agent /dev/null /etc/autoheal/hooks/views.token
  sudoedit /etc/autoheal/hooks/views.token
  ```

- **Any 2xx status means finished.** Any other status, including a redirect,
  means failed. Redirects are never followed.
- **The answer's body is ignored.** It is not stored, logged or sent to
  AutoHeal. AutoHeal learns only the status code and how long the call took.
- **Timeout.** The call stops at the hook's `timeout`, or sooner if the task
  allows less.
- **Make it safe to repeat.** A task runs at most once, but AutoHeal may send
  a new task for the same cache later.

AutoHeal does not take the hook's word, or the agent's, for the result. Once
AutoHeal has accepted the agent's report that the action finished, the agent
runs the health check named in the task and sends its result as telemetry;
AutoHeal decides from that check whether the recovery worked. After a failed
or refused task the check is not run.

### What the agent checks before acting

Every task must pass all of these, in this order, or it is refused and
nothing is done: a valid signature from a listed key; protocol version 1; not
expired, not issued more than 2 minutes ahead of this host's clock, and valid
for at most 10 minutes; addressed to this agent and its environment; never
seen before (task IDs are kept in `recovery-seen.json` in the state folder,
across restarts); an action and target enabled in the local config; and
arguments holding the target and nothing else.

AutoHeal can pause recovery from the dashboard. While it is paused, the agent
keeps asking and acts on nothing.

## 9. Upgrade

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

## 10. Uninstall

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

## 11. Troubleshooting

Logs are JSON lines; the `msg` field is quoted below.

| Log line | What it means | What to do |
|---|---|---|
| `AutoHeal refused this agent's credential; nothing will be sent until it is enrolled again with autoheal-agent enroll --force` | The credential was revoked, expired, or lacks a scope. Events keep spooling within the limits. | Create a new token on the dashboard and run `autoheal-agent enroll --force` as the service user. The running agent picks it up within 5 minutes. |
| `the agent was enrolled with a different API address; restart it to use the new one` | `enroll --force` pointed the agent at another AutoHeal address. | Restart the service. |
| `spool full: dropping the oldest events` | AutoHeal has been unreachable long enough to fill `spool.max_events` or `spool.max_mb`. | Fix the connection (see "sending telemetry failed"). Raise the spool limits if outages this long are expected. |
| `dropping events too old for AutoHeal to accept` | Spooled events passed 23 hours of age; AutoHeal accepts only the last 24 hours. | Nothing to do; the outage was longer than that. |
| `AutoHeal refused a batch as invalid; dropping it` | AutoHeal rejected the events (the `fields` show why). Most often the clock is far ahead. | Fix the clock (below). If it is right, report the fields to AutoHeal support. |
| `this host's clock differs from AutoHeal's; events dated more than 5 minutes ahead are refused` | The local time and AutoHeal's differ by more than 2 minutes. | Enable NTP (`timedatectl set-ntp true`, or System Settings → Date & Time on macOS). |
| `this agent is older than the minimum version AutoHeal supports; upgrade it` | AutoHeal still accepts this version but asks for an upgrade. Logged at most hourly. | Upgrade (section 9). |
| `sending telemetry failed; events stay in the spool and are retried with backoff` / `heartbeat failed; retrying with backoff` | AutoHeal could not be reached, timed out, or asked the agent to slow down. Retries back off up to 5 minutes. | Check DNS, the firewall and proxy (`HTTPS_PROXY` is honoured), and `ca_file` for an internal CA. `telemetry delivered again` / `heartbeat delivered again` confirm recovery. |
| `events are arriving faster than they can be saved; dropping results` | The disk holding the state folder is slow or full. | Free disk space on that volume. |
| `… can be read by other users (mode 0644); run: chmod 600 …` | The state folder, `credential.json` or a hook's `token_file` allows group or other access, so the agent refuses it. | Run the `chmod` it prints (`0700` for the folder, `0600` for the files), and keep them owned by `autoheal-agent`. |
| `refused a recovery task` | A task failed one of the checks in section 8; `reason` names which (`bad_signature`, `unknown_key`, `expired`, `not_enabled`, `replayed`, …). Nothing was done, and AutoHeal was told. | `unknown_key` or `bad_signature`: compare `recovery.platform_keys` with the server's key. `not_enabled`: the target is not declared in this config. `expired` or `not_yet_valid`: fix the clock. |
| `dropped a recovery task that cannot be reported` | A task was unreadable or badly signed and named no task to report against. Nothing was done. | Compare `recovery.platform_keys` with the server's key. If it persists, report it to AutoHeal support. |
| `this AutoHeal has no recovery support; asking again at a slower pace` | The server is older than the recovery feature. The agent asks every 10 minutes instead of every 15 seconds. | Nothing to do, or set `recovery.enabled: false`. |
| `the credential still lacks the recovery scope; …` | The credential was issued without the `recovery` scope and rotating it did not add it. Heartbeats and telemetry are not affected. | Check that recovery is available for this environment, or enroll again with `autoheal-agent enroll --force`. |
| `AutoHeal paused recovery; no task is acted on until it resumes` | Recovery was paused on the dashboard. | Nothing to do; `AutoHeal resumed recovery` follows when it is lifted. |
| `recovery task ended` with `stage=failed` | The action ran and failed: `hook_status` (the hook answered outside 2xx, or with a redirect), `hook_error` (it could not be reached, or its token file could not be read), `timeout`, or `check_error`. | Check the hook in your application and its `url`, `token_file` and `timeout`. |
| `could not run the verification check; AutoHeal will not see a fresh result` | The task named a check this agent does not run (for example after a rename). | Restore the check's name, or resolve the incident by hand. |

## 12. Security model

**What it sends**, only to the AutoHeal API address that issued its credential:

- at enrollment and on each heartbeat: the agent's name, hostname, version,
  operating system and architecture; and, on heartbeats while recovery is on,
  which of the two recovery actions are enabled (never their targets);
- check results: the check name and kind, up / degraded / down, the target
  (a sanitized URL, a host and port, or a disk path), the measured percentage
  or latency, a short error detail, and the agent version. URLs in results and logs
  are reduced to scheme, host, port and path; user information, query strings
  and fragments are dropped;
- for each recovery task: that it was received, started, and finished, failed
  or was refused, with a reason code, the duration, the hook's HTTP status or
  the check's status. Never the hook's address, its token or its answer.

It sends no file contents, process lists, environment variables or logs.

**What it never does:**

- It never runs commands or starts a process. Nothing in the binary can: a
  test builds every release target and fails if the linker kept any function
  that starts one (`os.StartProcess`, `os/exec`, `syscall.forkExec`,
  `syscall.Exec`), and a source scan forbids them and any way to skip TLS
  certificate checks.
- It takes no configuration from AutoHeal. What it checks, and which hooks
  exist, is decided only by the local config file. Answers from AutoHeal set
  only the heartbeat interval, credential rotation and the minimum version
  notice.
- With recovery off (the default) it takes no instructions at all and never
  asks for tasks. With recovery on, the only instructions it accepts are
  signed tasks for the two actions in section 8, on targets its own config
  names; every other task is refused. A task cannot carry a command, an
  address or a check definition: unknown fields are refused.
- A cache hook is only ever called on a loopback address, without a proxy.
- It does not follow redirects and always verifies TLS certificates (TLS 1.2 or
  later; your system's CAs plus an optional `ca_file`).
- It never logs the enrollment token, the credential or a hook token, and
  refuses a token passed as a command-line argument.

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
make release V=0.2.0
```

The version is stamped in at build time from the tag (or `make build
VERSION=0.2.0`); the source keeps `0.0.0-dev` as the default. Recovery tasks
arrive with 0.2.0.

`scripts/release.sh <version>` needs a clean tree and the tag `v<version>` on
`HEAD`. It builds the four targets with `CGO_ENABLED=0 -trimpath`, packs
reproducible archives into `dist/`, writes `dist/SHA256SUMS`, and signs it with
the minisign secret key file named by `MINISIGN_SECRET_KEY`. Without that
variable it stops after the checksums, signs nothing, and exits with status 3.

`scripts/test-install.sh <dist>` installs from those archives on a throwaway
Linux VM with systemd, against a stand-in API, and checks the result. CI runs
it on every push.

## Releasing

Pushing a tag `v<version>` runs [`.github/workflows/release.yml`](.github/workflows/release.yml):
`make check`, shellcheck, `scripts/release.sh`, a checksum (and signature)
check, the end-to-end install test, and then a GitHub release with the four
archives, `SHA256SUMS`, `SHA256SUMS.minisig` when signed, and `install.sh`.
A version with a suffix such as `0.3.0-rc.1` is published as a pre-release,
which `releases/latest` skips.

```bash
git tag -a v0.2.0 -m 'autoheal-agent 0.2.0'
git push origin v0.2.0
```

To sign releases, once:

1. Create a key without a password, so CI can use it: `minisign -G -W -p minisign.pub -s autoheal-release.key`.
2. Add the content of `autoheal-release.key` as the repository secret
   `MINISIGN_SECRET_KEY` (Settings → Secrets and variables → Actions). Keep
   an offline copy, and never commit it.
3. Commit `minisign.pub`. From the next tag, the workflow signs `SHA256SUMS`,
   fails if the secret is missing, and writes the public key into the
   released `install.sh`, which then refuses unsigned downloads.
