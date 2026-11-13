# AutoHeal agent

`autoheal-agent` is a small, single-binary service that watches one server for
AutoHeal. It reads the host's CPU, memory and disk use, runs the HTTP and TCP
checks you configure, and sends the results to your AutoHeal environment, where
a failing check opens an incident. It never starts a process.

## Install

On the server, as a user who can `sudo`:

```bash
curl -fsSL https://github.com/alimrun/autoheal-agent/releases/latest/download/install.sh | sudo sh
```

The script asks for the API address (from the environment's **Agents** page),
the agent's name, the service user and the enrollment token, then installs,
enrolls and starts the agent. It works on Linux (systemd) and macOS (launchd),
on `amd64` and `arm64`.

## Documentation

- [Installation](docs/installation.md): the installer and its options, automation, several projects on one server, manual install, upgrade, uninstall
- [Configuration](docs/configuration.md): every `agent.yaml` key
- [Command line](docs/cli.md): commands, flags, environment variables, exit codes
- [Recovery](docs/recovery.md): signed recovery tasks and cache hooks
- [Troubleshooting](docs/troubleshooting.md): log lines and error messages
- [Security](docs/security.md): what the agent sends and never does, release signatures
- [Changelog](CHANGELOG.md)

Building from source and releasing: [Development](docs/development.md).

<details>
<summary>Manual install (without the script)</summary>

Run these one step at a time; [Installation](docs/installation.md#manual-install) explains each one and lists the [requirements](docs/installation.md#requirements).
Download `SHA256SUMS` and `autoheal-agent_<version>_linux_amd64.tar.gz` or `autoheal-agent_<version>_linux_arm64.tar.gz` from the [latest release](https://github.com/alimrun/autoheal-agent/releases/latest).
On macOS use the `darwin_arm64` or `darwin_amd64` archive, the config `/usr/local/etc/autoheal/agent.yaml`, and the macOS block below.
Enrollment reads `AUTOHEAL_URL` (the API address, ending in `/api/v1`) and the token from standard input or `AUTOHEAL_ENROLLMENT_TOKEN`, never from an argument.

```bash
# 2. Verify
shasum -a 256 -c SHA256SUMS --ignore-missing
# 3. Install
tar -xzf autoheal-agent_<version>_linux_amd64.tar.gz
cd autoheal-agent_<version>_linux_amd64
sudo install -m 0755 autoheal-agent /usr/local/bin/autoheal-agent
autoheal-agent version
sudo useradd --system --no-create-home --home-dir /var/lib/autoheal-agent --shell /usr/sbin/nologin autoheal-agent
sudo install -d -m 0755 /etc/autoheal
sudo install -d -m 0700 -o autoheal-agent -g autoheal-agent /var/lib/autoheal-agent
# 4. Configure
sudo install -m 0644 packaging/agent.example.yaml /etc/autoheal/agent.yaml
sudoedit /etc/autoheal/agent.yaml
autoheal-agent validate
# 5. Enroll
printf '%s\n' '<token>' | sudo -u autoheal-agent env AUTOHEAL_URL='https://autoheal.example.com/api/v1' autoheal-agent enroll --token-stdin
# 6. Run as a service
sudo install -m 0644 packaging/autoheal-agent.service /etc/systemd/system/autoheal-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now autoheal-agent
# 7. Check the status
systemctl status autoheal-agent
journalctl -u autoheal-agent -f
```

Or enroll with the token in the environment:

```bash
sudo -u autoheal-agent env \
  AUTOHEAL_URL='https://autoheal.example.com/api/v1' \
  AUTOHEAL_ENROLLMENT_TOKEN='<token>' \
  autoheal-agent enroll
```

macOS user and folders (step 3), service (step 6) and status (step 7):

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
sudo install -m 0644 -o root -g wheel packaging/com.autoheal.agent.plist /Library/LaunchDaemons/com.autoheal.agent.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.autoheal.agent.plist
sudo launchctl print system/com.autoheal.agent | head -20
tail -f /usr/local/var/log/autoheal-agent.log
```
</details>
