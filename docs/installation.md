# Installation

- [Requirements](#requirements)
- [The installer](#the-installer)
- [Automation](#automation)
- [Several projects on one server](#several-projects-on-one-server)
- [Files and folders](#files-and-folders)
- [Manual install](#manual-install)
- [Upgrade](#upgrade)
- [Uninstall](#uninstall)

## Requirements

- Linux (`amd64` or `arm64`) with systemd, or macOS (Intel or Apple silicon)
  with launchd.
- Outbound HTTPS from the server to your AutoHeal address.
- A reasonably accurate clock (NTP). AutoHeal refuses events dated more than
  5 minutes in the future.
- An enrollment token, created on the environment's **Agents** page in the
  AutoHeal dashboard. It is shown once.

No other software is needed: the binary is statically linked.

> **Releases are unsigned previews for now.** Each release carries a
> `SHA256SUMS` file, and the installer refuses an archive that does not match
> it. The release key for a minisign signature does not exist yet; see
> [Release signatures](security.md#release-signatures).

## The installer

On the server, as a user who can `sudo`:

```bash
curl -fsSL https://github.com/alimrun/autoheal-agent/releases/latest/download/install.sh | sudo sh
```

The environment's **Agents** page shows the same command with `--url` already
filled in. To read the script before you run it, download it, read it, then run
`sudo sh install.sh`.

### What it asks

The script asks on the terminal for anything you did not pass as an option:

1. the API address, ending in `/api/v1`. It must start with `https://`;
   `http://` is allowed only for `localhost`, `127.0.0.1` or `[::1]`;
2. the agent's name on the dashboard (the host name by default). It is asked
   only when the script writes a new config;
3. the system user the service runs as (`autoheal-agent` by default). Root is
   refused;
4. the enrollment token from the dashboard. It is read with echo off, never
   from the command line or the environment, never printed, and never written
   to your shell history. You get three tries; an empty answer counts as one.

It asks for the token only when this agent is not enrolled yet, or when you
pass `--reenroll`.

### What it checks

Before it changes anything, the script:

- checks that it runs as root;
- detects the operating system, the distribution and the architecture
  (`amd64` or `arm64`);
- checks for the tools it needs: curl or wget, `sha256sum` or `shasum`, tar,
  mktemp and install; on Linux also gzip, useradd, groupadd, and runuser or
  sudo; and minisign once releases are signed. For anything missing it prints
  the install command for your system (`apt-get`, `dnf` or `yum`, `zypper`,
  `apk`, `pacman`, or `brew` on macOS);
- on Linux, checks that systemd is running, unless you pass `--no-start`;
- downloads `SHA256SUMS` and the archive for this platform, and refuses the
  archive on a checksum mismatch. Once releases are signed it also refuses a
  missing or bad minisign signature on `SHA256SUMS`.

It then shows what it will do (system, version, API address, config, service
user, enrollment and service) and asks `Install? [Y/n]`.

### What it does

1. Creates the service user if it does not exist (on Linux with a group of the
   same name; on macOS with a free ID between 400 and 499).
2. Installs the binary to `/usr/local/bin/autoheal-agent`.
3. Creates the config folder (`0755`) and the state folder (`0700`, owned by
   the service user), and puts `agent.example.yaml` in the config folder.
4. Writes a starter config that reports CPU, memory and root disk use, with no
   HTTP or TCP checks. An existing config is kept. It then runs
   `autoheal-agent validate` on the config.
5. Enrolls as the service user with `autoheal-agent enroll --token-stdin`. The
   token reaches the agent only through a pipe. The `HTTPS_PROXY` and
   `NO_PROXY` variables of the shell that runs the script are passed on to
   `enroll`.
6. Installs, enables and starts the systemd unit, or the launchd daemon on
   macOS, and checks that it is running.
7. Prints a summary and the service status.

After that, add your HTTP and TCP checks to the config
([Configuration](configuration.md)), run `autoheal-agent validate`, and restart
the service.

## Automation

Without a terminal (Ansible, cloud-init, CI), pass every input as an option,
`--yes`, and the token on standard input from a file only root can read:

```bash
sudo sh install.sh --url https://autoheal.example.com/api/v1 --name web-01 --yes --token-stdin < /root/autoheal-token
```

The script refuses to run without a terminal unless `--url`, `--token-stdin`
and `--yes` are given (`--token-stdin` is not needed when the agent is already
enrolled). It reads the first line of standard input as the token, and stops
at once if enrollment fails. The options can also come from the environment
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
| `--no-start` | | Install and enroll, but do not install, enable or start the service. |
| `--download-base URL` | | Fetch the release files from a mirror. |
| `-h`, `--help` | | Print the options and stop. |

Every option that takes a value also accepts the `--option=value` form.
`AUTOHEAL_YES` also accepts `true` or `yes`. `--download-base` must be
`https://`, `file://`, or `http://` to `localhost` or `127.0.0.1`; the script
fetches `SHA256SUMS` and the archive from that address.

With `--no-start`, the script does not need systemd and does not write a unit
or a plist. Install the service yourself ([step 6](#6-run-as-a-service)).

The script exits with status 0 when it is done, 1 when it stops on an error or
when you answer no, and 130 when it is interrupted. Its messages are listed in
[Troubleshooting](troubleshooting.md#installer-messages).

## Several projects on one server

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

When the script asks for the short name, use lower-case letters, digits and
`-`, starting with a letter or digit, up to 30 characters. `default`, `agent`
and `agent.example` are taken. In automation, pass `--instance NAME` instead.
To upgrade or re-enroll that agent later, pass the same `--instance NAME`.

The script lists every agent already on the server, with the agent, project and
environment each one joined. All of them run as one service user: `--user`
with another user is refused while other agents exist.

Each instance has its own log: `journalctl -u autoheal-agent-<instance>` on
Linux, `/usr/local/var/log/autoheal-agent-<instance>.log` on macOS.

## Files and folders

| | Linux | macOS |
|---|---|---|
| Binary | `/usr/local/bin/autoheal-agent` | `/usr/local/bin/autoheal-agent` |
| Config | `/etc/autoheal/agent.yaml` | `/usr/local/etc/autoheal/agent.yaml` |
| State folder (`0700`) | `/var/lib/autoheal-agent` | `/usr/local/var/autoheal-agent` |
| Credential (`0600`) | `/var/lib/autoheal-agent/credential.json` | `/usr/local/var/autoheal-agent/credential.json` |
| Service | `/etc/systemd/system/autoheal-agent.service` | `/Library/LaunchDaemons/com.autoheal.agent.plist` |
| Logs | the journal (`journalctl -u autoheal-agent`) | `/usr/local/var/log/autoheal-agent.log` |

The state folder holds the credential (`credential.json`), the spool of events
not sent yet (`spool/`), and, with recovery on, the task IDs already received
(`recovery-seen.json`). It must be readable only by the service user: the
agent refuses to start if the folder or the credential file is open to group
or other users.

## Manual install

These are the steps the installer runs, for a server where you would rather do
them by hand. The README keeps the commands alone in its collapsed
"Manual install" block.

### 1. Requirements

Besides the [requirements](#requirements), you need `shasum` (or
`sha256sum`) to check the download, and `minisign` to check the signature once
releases are signed.

### 2. Download and verify

From the [release](https://github.com/alimrun/autoheal-agent/releases), download
the archive for your platform, `SHA256SUMS` and, once releases are signed,
`SHA256SUMS.minisig`:

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

Once releases are signed, check that the checksums were published by AutoHeal.
`<key>` is the key line from `minisign.pub` in this repository; get it from the
repository, not from the release page:

```bash
minisign -Vm SHA256SUMS -P <key>
```

Both must succeed. If either fails, do not install the archive.

Each archive holds the binary, the README and the `packaging/` folder with the
example config, the systemd unit and the launchd plist.

### 3. Install

Unpack the archive and install the binary (Linux shown; on macOS use
`darwin_arm64` or `darwin_amd64`):

```bash
tar -xzf autoheal-agent_<version>_linux_amd64.tar.gz
cd autoheal-agent_<version>_linux_amd64
sudo install -m 0755 autoheal-agent /usr/local/bin/autoheal-agent
autoheal-agent version
```

The agent runs as its own unprivileged user, `autoheal-agent`, which owns the
state folder ([Files and folders](#files-and-folders)).

**Linux:**

```bash
sudo useradd --system --no-create-home --home-dir /var/lib/autoheal-agent --shell /usr/sbin/nologin autoheal-agent
sudo install -d -m 0755 /etc/autoheal
sudo install -d -m 0700 -o autoheal-agent -g autoheal-agent /var/lib/autoheal-agent
```

**macOS** (pick a free ID below 500 for the user and group;
`dscl . -list /Users UniqueID` shows the ones in use):

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

### 4. Configure

Copy the example and edit it. [Configuration](configuration.md) describes every
key.

```bash
sudo install -m 0644 packaging/agent.example.yaml /etc/autoheal/agent.yaml   # macOS: /usr/local/etc/autoheal/agent.yaml
sudoedit /etc/autoheal/agent.yaml
autoheal-agent validate                                                       # or: --config PATH
```

`validate` prints every problem with its line number and changes nothing. The
config never holds the AutoHeal address or a secret; those come from
enrollment.

### 5. Enroll

Enrollment trades the one-time token for the agent's own credential. Run it
once, as the service user, with two values from the dashboard's token page:

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
in the process list; `enroll` refuses one. Add `--config PATH` if the config is
not at the default path.

`enroll` writes `credential.json` into the state folder with mode `0600` and
prints the agent, project and environment it joined. If the agent is already
enrolled it stops. To replace the credential, for example after the old one was
revoked, run it again with `--force` and a new token. A running agent picks up
the new credential at its next heartbeat; if AutoHeal had refused the old one,
within 5 minutes. [Command line](cli.md#enroll) lists every option and error.

### 6. Run as a service

**Linux (systemd):**

```bash
sudo install -m 0644 packaging/autoheal-agent.service /etc/systemd/system/autoheal-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now autoheal-agent
```

The unit runs `autoheal-agent run` as `autoheal-agent`, restarts it 10 seconds
after a failure, and sandboxes it (`NoNewPrivileges`, `ProtectSystem=strict`,
`ProtectHome`, `PrivateTmp`, no capabilities, IP and Unix sockets only). Only
`/var/lib/autoheal-agent` is writable.

**macOS (launchd):**

```bash
sudo install -m 0644 -o root -g wheel packaging/com.autoheal.agent.plist /Library/LaunchDaemons/com.autoheal.agent.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.autoheal.agent.plist
```

launchd starts it at boot and restarts it after a crash or a non-zero exit, at
most every 10 seconds. Its output goes to
`/usr/local/var/log/autoheal-agent.log`.

To run it in the foreground instead, for a first test:
`sudo -u autoheal-agent autoheal-agent run`. It logs JSON lines to standard
error and stops cleanly on `Ctrl-C` or `SIGTERM`, saving events it has not sent
yet and trying for 5 seconds to send them.

### 7. Check the status

**Linux:**

```bash
systemctl status autoheal-agent
journalctl -u autoheal-agent -f
```

**macOS:**

```bash
sudo launchctl print system/com.autoheal.agent | head -20
tail -f /usr/local/var/log/autoheal-agent.log
```

The first log line is `agent started`, with the agent, project and environment
it reports to and the number of checks.

Open the environment's **Agents** page. Within a minute of starting, the agent
shows as online with its hostname, version and platform, and its last
heartbeat. Its checks appear as `<host_prefix>:<check name>` (for example
`web-01:api`, `web-01:host.cpu`) once a status has held for two runs in a row:
about one interval after start for most checks, and two for CPU, whose first
run only takes a sample. A check that goes down opens an
incident; it resolves when the check is up again. An **Update required** marker
means this version is older than the minimum AutoHeal supports.

## Upgrade

Run the installer again. It installs the new binary, keeps the config and the
credential, and restarts every agent on the server:

```bash
curl -fsSL https://github.com/alimrun/autoheal-agent/releases/latest/download/install.sh | sudo sh
```

On a terminal it asks for the API address again, to check the saved credential
belongs to it, and, when the server is already enrolled, whether to add an
agent for another project (answer `n` to just upgrade). Pass `--version X.Y.Z`
for a given release. With `--no-start` it does not restart anything.

By hand, verify the new download first ([step 2](#2-download-and-verify)),
then:

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

The config, the credential and the spool stay where they are. Events spooled
while the agent was stopped are sent when it starts, and the dashboard shows
the new version on the next heartbeat. Restart every other instance on the
server too, so each one runs the new binary.

## Uninstall

First **revoke the agent in the dashboard** (on the environment's **Agents**
page), so its credential stops working even if a copy of it survives. Then:

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

Removing `/etc/autoheal` (or `/usr/local/etc/autoheal`) removes the config of
every agent on the server. If you used another service user, use its name.

To remove only one further agent, `billing-prod` for example, revoke it in the
dashboard and keep the binary and the user:

```bash
# Linux
sudo systemctl disable --now autoheal-agent-billing-prod
sudo rm /etc/systemd/system/autoheal-agent-billing-prod.service
sudo systemctl daemon-reload
sudo rm -r /var/lib/autoheal-agent-billing-prod /etc/autoheal/billing-prod.yaml

# macOS
sudo launchctl bootout system/com.autoheal.agent.billing-prod
sudo rm /Library/LaunchDaemons/com.autoheal.agent.billing-prod.plist
sudo rm -r /usr/local/var/autoheal-agent-billing-prod /usr/local/etc/autoheal/billing-prod.yaml
sudo rm /usr/local/var/log/autoheal-agent-billing-prod.log
```
