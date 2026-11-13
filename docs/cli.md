# Command line

```
autoheal-agent enroll   [--config PATH] [--token-stdin] [--force]
autoheal-agent run      [--config PATH]
autoheal-agent validate [--config PATH]
autoheal-agent version
```

- [Common flag](#common-flag)
- [enroll](#enroll)
- [run](#run)
- [validate](#validate)
- [version and help](#version-and-help)
- [Environment variables](#environment-variables)
- [Exit codes](#exit-codes)
- [The installer](#the-installer)

Flags may be written with one or two dashes, and with a space or `=` before the
value: `--config PATH`, `--config=PATH`, `-config PATH`. Flags come after the
command.

## Common flag

| Flag | Default | |
|---|---|---|
| `--config PATH` | `/etc/autoheal/agent.yaml` on Linux, `/usr/local/etc/autoheal/agent.yaml` on macOS | The [configuration file](configuration.md). `enroll`, `run` and `validate` take it. |

## enroll

```bash
printf '%s\n' '<token>' | sudo -u autoheal-agent env AUTOHEAL_URL='https://autoheal.example.com/api/v1' autoheal-agent enroll --token-stdin
```

Trades a one-time enrollment token for the agent's own credential. Run it as
the service user, so the credential belongs to that user.

| Flag | |
|---|---|
| `--config PATH` | The configuration file. `enroll` reads the state folder, `ca_file` and `name` from it. |
| `--token-stdin` | Read the token from the first line of standard input instead of `AUTOHEAL_ENROLLMENT_TOKEN`. |
| `--force` | Replace an existing credential. |

It reads:

- `AUTOHEAL_URL`: the API address. It must be `https://`, or `http://` to
  `localhost`, `127.0.0.1` or `::1`, with no user name, password, query or
  fragment. If the path does not end in `/api/v1`, `enroll` adds it.
- the token, from `AUTOHEAL_ENROLLMENT_TOKEN`, or from standard input with
  `--token-stdin`. Spaces and line breaks around it are removed. A value longer
  than 64 characters is refused as not a token.

In this order, it:

1. refuses any argument after the flags, so the token is never on the command
   line, where other users can see it in the process list;
2. loads and validates the config;
3. checks `AUTOHEAL_URL`;
4. stops if the state folder already holds `credential.json`, unless you pass
   `--force`;
5. reads the token;
6. creates the state folder with mode `0700`, or checks an existing one is not
   open to group or other users;
7. registers with AutoHeal, sending the agent's name (if the config sets one),
   hostname, version, operating system and architecture. It gives up after 20
   seconds;
8. writes `credential.json` (mode `0600`) with the API address, the credential,
   and the agent, project and environment it joined.

On success it prints:

```
Enrolled "web-01" into Shop · production.
Start the agent with: autoheal-agent run
```

If this version is older than the minimum AutoHeal supports, it also prints a
warning on standard error, and still succeeds.

With `--force`, a running agent picks up the new credential at its next
heartbeat, or within 5 minutes if AutoHeal had refused the old one. If the new
credential is for another API address, restart the agent.
[Troubleshooting](troubleshooting.md#enroll) lists every error.

## run

```bash
sudo -u autoheal-agent autoheal-agent run
```

Runs the agent in the foreground until it gets `SIGTERM` or `SIGINT`
(`Ctrl-C`). The systemd unit and the launchd daemon run this command.

On start it loads the config, the credential, the spool and, with recovery on,
the replay store. If any of these fails it logs `cannot start` with the reason
and exits with status 1. It then logs `agent started`, sends heartbeats, runs
its checks, sends their results, and, with recovery on, asks for recovery tasks
every 15 seconds.

On `SIGTERM` or `SIGINT` it stops its checks, saves the events not sent yet to
the spool, tries for up to 5 seconds to send them, logs `agent stopped` and
exits with status 0.

Logs are JSON lines on standard error, at the config's `log_level`. Each line
has `time`, `level` and `msg`, and fields such as `error`, `check` or `task`.

## validate

```bash
autoheal-agent validate --config /etc/autoheal/agent.yaml
```

Loads the configuration file, applies the defaults and checks every key. It
changes nothing and contacts nothing. On success it prints, for example:

```
/etc/autoheal/agent.yaml is valid: 2 checks, host checks on, recovery off
```

On failure it prints every problem, one per line, as
`<file>:<line>: <key>: <message>`, and exits with status 1:

```
/etc/autoheal/agent.yaml:7: spool.max_events: must be between 50 and 1000000
/etc/autoheal/agent.yaml:9: host.interval: must be between 10s and 1h
```

It also checks that `ca_file` holds a PEM certificate and that every hook's
`token_file` exists with owner-only access, so run it as root or as the
service user.

## version and help

`autoheal-agent version` (also `--version` or `-v`) prints the version, the
commit, the build time and the platform:

```
autoheal-agent 0.2.0 (commit 5a496da4b304, built 2026-11-13T17:06:49+06:00, linux/amd64)
```

A build from source without a tag reports `0.0.0-dev`.

`autoheal-agent help` (also `--help` or `-h`) prints the usage on standard
output.

## Environment variables

| Variable | Used by | |
|---|---|---|
| `AUTOHEAL_URL` | `enroll` | The API address. `run` uses the address saved in the credential instead. |
| `AUTOHEAL_ENROLLMENT_TOKEN` | `enroll` | The token, when `--token-stdin` is not given. |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (or lower case) | `enroll`, `run` | A proxy for requests to the AutoHeal API. HTTP checks and cache hooks never use a proxy. |

The systemd unit and the launchd plist set no environment. To use a proxy under
systemd, add it with `sudo systemctl edit autoheal-agent`:

```ini
[Service]
Environment=HTTPS_PROXY=http://proxy.example.com:3128
```

On macOS, add an `EnvironmentVariables` dictionary to the plist.

## Exit codes

| Code | When |
|---|---|
| `0` | Success. For `run`, a clean stop after `SIGTERM` or `SIGINT`. |
| `1` | The command failed: an invalid or missing config, a bad `AUTOHEAL_URL`, an existing credential without `--force`, a refused token, a failed enrollment, or, for `run`, anything that stops it from starting. |
| `2` | A usage error: no command, an unknown command, an unknown flag, `-h` after a command, an argument after the flags. |

## The installer

`install.sh` has its own options, listed under
[Automation](installation.md#automation). It exits with status 0 when it is
done, 1 on an error or when you answer no, and 130 when interrupted.
