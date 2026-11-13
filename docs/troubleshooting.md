# Troubleshooting

- [Where the logs are](#where-the-logs-are)
- [The agent does not start](#the-agent-does-not-start)
- [Log lines while running](#log-lines-while-running)
- [Recovery log lines](#recovery-log-lines)
- [enroll](#enroll)
- [validate](#validate)
- [Installer messages](#installer-messages)

## Where the logs are

| | Command |
|---|---|
| Linux | `journalctl -u autoheal-agent -f` (a further agent: `-u autoheal-agent-<instance>`) |
| macOS | `tail -f /usr/local/var/log/autoheal-agent.log` (a further agent: `autoheal-agent-<instance>.log`) |

Logs are JSON lines. The `msg` field is quoted below; other fields, such as
`error`, `reason` or `check`, give the details. Set `log_level: debug` in the
config and restart for more detail.

## The agent does not start

`run` logs `cannot start` with an `error` field, and exits with status 1.
systemd tries again after 10 seconds.

| `error` | What to do |
|---|---|
| ``this agent is not enrolled; run `autoheal-agent enroll` first`` | [Enroll](installation.md#5-enroll) as the service user. |
| `<folder> can be read by other users (mode 0755); run: chmod 700 <folder>` | The state folder or `credential.json` allows group or other access, so the agent refuses it. Run the `chmod` it prints (`700` for the folder, `600` for the file) and keep them owned by the service user. |
| `<file> is not a valid credential file: …` or `… is missing its token or API address; enroll again` | Revoke the agent on the dashboard and enroll again with `--force`. |
| `reading CA file: …` or `<file> holds no PEM certificate` | Fix `ca_file` in the config. |
| `creating spool: …` | The state folder is not writable by the service user. Check its owner, and on Linux that the unit's `ReadWritePaths` matches `state_dir`. |
| `<file> is not a valid replay store: …` | `recovery-seen.json` in the state folder is damaged. Stop the agent and remove it; a replayed task can then be accepted only until it expires. |

A config problem is printed as plain text before logging starts, as
`<file>:<line>: <key>: <message>`. Run `autoheal-agent validate` to see all of
them ([validate](#validate)).

## Log lines while running

| `msg` | What it means | What to do |
|---|---|---|
| ``AutoHeal refused this agent's credential; nothing will be sent until it is enrolled again with `autoheal-agent enroll --force` `` | The credential was revoked, expired, or lacks a scope. Events keep spooling within the limits. | Create a new token on the dashboard and run `enroll --force` as the service user (below). The running agent picks it up within 5 minutes. |
| `found a new credential; resuming` | The agent picked up a credential written by `enroll --force`. | Nothing to do. |
| `the agent was enrolled with a different API address; restart it to use the new one` | `enroll --force` pointed the agent at another AutoHeal address. | Restart the service. |
| `the credential file changed but cannot be used` | `credential.json` was replaced by a file the agent refuses, often for its mode. | See [The agent does not start](#the-agent-does-not-start). |
| `spool full: dropping the oldest events` | AutoHeal has been unreachable long enough to fill `spool.max_events` or `spool.max_mb`. | Fix the connection (see "sending telemetry failed"). Raise the spool limits if outages this long are expected. |
| `dropping events too old for AutoHeal to accept` | Spooled events passed 23 hours of age; AutoHeal accepts only the last 24 hours. | Nothing to do; the outage was longer than that. |
| `AutoHeal refused a batch as invalid; dropping it` | AutoHeal rejected the events (the `fields` show why). Most often the clock is far ahead. | Fix the clock (below). If it is right, report the fields to AutoHeal support. |
| `this host's clock differs from AutoHeal's; events dated more than 5 minutes ahead are refused` | The local time and AutoHeal's differ by more than 2 minutes. Logged at most hourly. | Enable NTP (`timedatectl set-ntp true`, or System Settings, Date & Time on macOS). |
| `this agent is older than the minimum version AutoHeal supports; upgrade it` | AutoHeal still accepts this version but asks for an upgrade. Logged at most hourly. | [Upgrade](installation.md#upgrade). |
| `sending telemetry failed; events stay in the spool and are retried with backoff` / `heartbeat failed; retrying with backoff` | AutoHeal could not be reached, timed out, refused access to the environment, or asked the agent to slow down. Retries back off up to 5 minutes. | Check DNS, the firewall and the proxy ([`HTTPS_PROXY`](cli.md#environment-variables)), and `ca_file` for an internal CA. `telemetry delivered again` / `heartbeat delivered again` confirm recovery. |
| `credential rotation failed; the next heartbeat will try again` | AutoHeal asked for a rotation and it did not go through. The current credential keeps working. | Nothing to do unless it repeats; then check the connection. |
| `could not save the rotated credential; keeping the current one` | The state folder could not be written. | Free disk space and check the state folder's owner and mode. |
| `events are arriving faster than they can be saved; dropping results` | Results queue up faster than the spool can write them. The disk holding the state folder is slow or full. | Free disk space on that volume. |
| `could not save events to the spool; they are lost` | The spool could not be written. | Free disk space and check the state folder's owner and mode. |
| `spool batch could not be read and was set aside` | A damaged batch was moved to `spool/corrupt/`, where the last 10 are kept. | Nothing to do. |

To enroll again as the service user:

```bash
printf '%s\n' '<token>' | sudo -u autoheal-agent env AUTOHEAL_URL='https://autoheal.example.com/api/v1' autoheal-agent enroll --token-stdin --force
```

Or run the installer again with `--reenroll`.

## Recovery log lines

| `msg` | What it means | What to do |
|---|---|---|
| `refused a recovery task` | A task failed one of the [checks](recovery.md#what-the-agent-checks-before-acting); `reason` names which (`bad_signature`, `unknown_key`, `expired`, `not_enabled`, `replayed`, …). Nothing was done, and AutoHeal was told. | `unknown_key` or `bad_signature`: compare `recovery.platform_keys` with the server's key. `not_enabled`: the action or target is not declared in this config. `expired` or `not_yet_valid`: fix the clock. |
| `dropped a recovery task that cannot be reported` | A task was unreadable or badly signed and named no task to report against. Nothing was done. | Compare `recovery.platform_keys` with the server's key. If it persists, report it to AutoHeal support. |
| `this AutoHeal has no recovery support; asking again at a slower pace` | The server is older than the recovery feature. The agent asks every 10 minutes instead of every 15 seconds. | Nothing to do, or set `recovery.enabled: false`. |
| `the credential lacks the recovery scope; rotating it once` | The credential was issued without the `recovery` scope. | Nothing to do; the rotation usually adds it. |
| `the credential still lacks the recovery scope; …` | Rotating did not add the scope. Heartbeats and telemetry are not affected. | Check that recovery is available for this environment, or enroll again with `--force`. |
| `fetching recovery tasks failed; retrying with backoff` | AutoHeal could not be reached for tasks. | As for "sending telemetry failed". |
| `AutoHeal paused recovery; no task is acted on until it resumes` | Recovery was paused on the dashboard. | Nothing to do; `AutoHeal resumed recovery` follows when it is lifted. |
| `running a recovery task` | A verified task is running; `action` and `target` say which. | Nothing to do. |
| `recovery task ended` with `stage=failed` | The action ran and failed: `hook_status` (the hook answered outside 2xx, or with a redirect), `hook_error` (it could not be reached, or its token file could not be read), `timeout`, or `check_error`. | Check the hook in your application and its `url`, `token_file` and `timeout`. |
| `could not run the verification check; AutoHeal will not see a fresh result` | The task named a check this agent does not run (for example after a rename), or the check had nothing to report yet (the CPU check's first run). | Restore the check's name, or resolve the incident by hand. |
| `AutoHeal did not accept a recovery task report` | A report was refused, or AutoHeal could not be reached for a minute. | If it repeats, check the connection and the dashboard. |
| `could not save the replay store; the recovery task is not acted on` | `recovery-seen.json` could not be written. | Free disk space and check the state folder. |

## enroll

`enroll` prints its errors on standard error and exits with status 1 (2 for an
argument).

| Message | What to do |
|---|---|
| `the enrollment token must not be passed as an argument, where other users can see it; set AUTOHEAL_ENROLLMENT_TOKEN or use --token-stdin` | Pass the token in the environment or on standard input. |
| `set AUTOHEAL_URL to the API address shown on the enrollment token page` | Set `AUTOHEAL_URL`. `--force` needs it too. |
| `"…" is not an http or https address` | Copy the API address again from the dashboard. |
| `the API address must not contain a user name, password, query or fragment` | Use the address exactly as the dashboard shows it. |
| `the API address must use HTTPS; plain HTTP is allowed only to localhost, not …` | Use the `https://` address. |
| `configuration file … not found; copy packaging/agent.example.yaml there and edit it` | [Configure](installation.md#4-configure) first, or pass `--config PATH`. |
| `this agent is already enrolled (… exists); run with --force to replace its credential` | Add `--force` if you mean to replace it. |
| `no enrollment token: set AUTOHEAL_ENROLLMENT_TOKEN or pipe it to --token-stdin` | Give the token. |
| `that is not an enrollment token; copy it again from the token page` | The value is longer than 64 characters. Copy the token again. |
| `… can be read by other users (mode …); run: chmod 700 …` | Run the `chmod` it prints on the state folder. |
| `the enrollment token was refused: it may have expired, been cancelled or been used already; create a new one on the environment's page` | Create a new token. |
| `enrollment failed: …` | AutoHeal could not be reached or refused the request; the rest of the line says why. Check the address, DNS, the firewall, the proxy and `ca_file`. |
| `enrolled, but the credential could not be saved: …` | Revoke this agent on the dashboard, fix the state folder, and enroll again. |
| `warning: this agent (…) is older than the minimum version AutoHeal supports (…); upgrade it` | Enrollment worked. [Upgrade](installation.md#upgrade) soon. |

## validate

`validate` prints one line per problem, as `<file>:<line>: <key>: <message>`.
The message says what the key must be; [Configuration](configuration.md) lists
every rule. Two messages need more explanation:

| Message | What to do |
|---|---|
| `url: the API address comes from enrollment and cannot be set here; remove this key` | Remove `url`, `api_url` or `endpoint` from the config. The address is set at enrollment. |
| `line …: field … not found in …` | The key is misspelt or not in this version. Check it against [Configuration](configuration.md). |

## Installer messages

The installer prints its messages as `autoheal: …`, and errors as
`autoheal: error: …`. It stops at the first error. Unless the message says
otherwise, an error before `Install? [Y/n]` has changed nothing.

| Message | What to do |
|---|---|
| `run this as root, for example: curl -fsSL … \| sudo sh` | Run it with `sudo`. |
| `unknown option: … (see --help)` or `--url needs a value` | Check the options under [Automation](installation.md#automation). |
| `the enrollment token is never passed as an argument, where other users can see it; the script asks for it` | Remove the token from the command line. Let the script ask, or use `--token-stdin`. |
| `ignoring AUTOHEAL_ENROLLMENT_TOKEN: the script asks for the token, or reads it with --token-stdin` | A warning. The script asks for the token instead. |
| `--url: the API address must start with https:// (http:// is allowed only for localhost, 127.0.0.1 or [::1])` | Copy the API address from the environment's **Agents** page. |
| `--url: the API address holds characters an address does not use` | Copy the address again, without spaces or quotes. |
| `--user: the agent never runs as root; choose another user` / `--user: … has user ID 0, which is root` | Choose an unprivileged user, or keep the default. |
| `--user: a user name is lower-case letters, digits, '_' and '-', starting with a letter or '_'` / `--user: a user name is at most 32 characters` | Choose a valid name. |
| `--user: the other agents on this server run as …, and all of them share one service user` | Leave out `--user`, or use the user named. |
| `--name: the name may not hold quotes, backslashes or control characters` | Choose another name. |
| `--instance: an instance name is lower-case letters, digits and '-', starting with a letter or digit` / `--instance: an instance name is at most 30 characters` / `--instance: '…' is taken by the agent's own files; choose another name` | Choose another instance name. |
| `--download-base must be https://, file:// or http:// to localhost` | Fix the mirror address. |
| `--version must look like 1.2.3 (got '…')` | Pass a version such as `0.2.0`. |
| `unsupported operating system …; the agent runs on Linux and macOS` / `unsupported architecture …; the agent runs on amd64 and arm64` | The agent does not run on this system. |
| `missing tools: …. Install them with: … (minisign may need EPEL on RHEL), then run this again` | Run the install command it prints, then the script again. |
| `systemd is not running here; install the service yourself (docs/installation.md, "6. Run as a service") and run again with --no-start` | Run with `--no-start` and start the agent another way ([step 6](installation.md#6-run-as-a-service) shows the systemd and launchd forms). |
| `no terminal to ask on, so pass every input as an option (missing: …); nothing was changed` | Pass the options it names ([Automation](installation.md#automation)). |
| `no answer; stopped, and nothing was changed` | The terminal closed while it was asking. Run it again. |
| `no enrollment token was given; nothing was changed` | Paste the token when asked, or check the file you pass to `--token-stdin`. |
| `could not download …/SHA256SUMS` / `could not download …` | Check the network, the proxy, `--version` and `--download-base`. |
| `SHA256SUMS lists no archive for …` | The release has no archive for this platform. |
| `asked for …, but the release lists …` | `--version` and `--download-base` point at different releases. |
| `this release is not signed yet; verifying the checksum only` | A warning while releases are unsigned. The checksum is still verified. |
| `this release has no SHA256SUMS.minisig; refusing an unsigned download` / `the signature on SHA256SUMS does not verify; refusing to install` | Do not install this download. Try again later, or report it. |
| `checksum mismatch for … (expected …, got …); refusing to install` | The download is damaged or was changed. Do not install it; run the script again. |
| `the archive does not hold autoheal-agent` | The archive is not an agent release. |
| `stopped; nothing was changed` | You answered no to `Install? [Y/n]`. |
| `there is already an agent called … here; run with --instance … to upgrade it` / `'default' is the first agent; choose another name` | Choose another short name, or upgrade that agent with `--instance`. |
| `keeping the existing config …; --name applies only to a new config (edit its name: line instead)` | A warning. Edit `name:` in the config if you want to change it. |
| `no free ID between 400 and 499 for the … group` (or `user`) | On macOS, create the user and group yourself ([step 3](installation.md#3-install)) and run the script again; it uses an existing user. |
| `the primary group of … ('…') is not a plain group name` | Use another service user. |
| `the starter config is not valid (is the name longer than 80 characters?); run again with another --name` | Choose a shorter name. The binary is already installed; the next run upgrades it. |
| `the config … is not valid; fix it and run the script again` | Fix the problems `validate` printed above it. |
| `runuser or sudo is needed to enroll as …` | Install `util-linux` (runuser) or sudo. |
| `try again with a token from the dashboard (attempt … of 3)` | Enrollment failed; the reason is printed above it. Paste a new token. |
| `enrollment failed; create a new token on the dashboard and run the script again` | Read the `enroll` error above it ([enroll](#enroll)). The binary, user and config are installed; run the script again with a new token. |
| `the saved credential may be for another API address; run again with --reenroll and a new token to switch` | A warning. `--url` differs from the address the agent enrolled with. |
| `the service did not start; see: journalctl -u … -n 50` (or `tail -n 50 …` on macOS) | Read the log it names ([The agent does not start](#the-agent-does-not-start)). |
| `launchctl could not load …` | Check the plist and `sudo launchctl print system/com.autoheal.agent`. |
| `could not restart the agent …` | A warning. Restart that agent's service by hand so it runs the new binary. |
