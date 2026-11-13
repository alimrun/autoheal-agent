# Changelog

Each version of the agent and what changed in it. Versions follow
[semantic versioning](https://semver.org/).

## 0.2.0 (2026-11-13)

### Added

- Recovery, off by default. With `recovery.enabled: true`, the agent asks
  AutoHeal for signed recovery tasks every 15 seconds and runs two actions, only
  on targets its config declares: `recheck_health` runs a configured check once,
  and `clear_cache` sends one `POST` to a cache hook on a loopback address. See
  [Recovery](docs/recovery.md).
- Each task is checked before it runs: its Ed25519 signature against
  `recovery.platform_keys`, the protocol version, its time window, the agent and
  environment it is addressed to, a replay store in `recovery-seen.json`, and
  the local config. The agent reports each task as received, running, and
  finished, failed or refused, then runs the task's verification check.
- New config keys under `recovery`: `enabled`, `platform_keys`, and
  `actions.recheck_health.checks` and `actions.clear_cache.hooks` (with `name`,
  `url`, `token_file` and `timeout`).
- Heartbeats name the recovery actions that are enabled, never their targets.
- A credential that lacks the `recovery` scope is rotated once to get it.
- A one-line installer, `install.sh`, attached to every release. It asks on
  the terminal for the API address, the agent's name, the service user and the
  enrollment token (with echo off), checks the system and its tools, verifies
  the download against `SHA256SUMS`, shows what it will do and asks before
  changing anything. It installs the binary, user, folders, a starter config
  and the service, enrolls, starts the agent and prints its status. Running it
  again upgrades the binary and keeps the config and the credential.
- Installer options for automation (`--url`, `--name`, `--user`, `--yes`,
  `--token-stdin`, `--reenroll`, `--version`, `--no-start`,
  `--download-base`), also read from `AUTOHEAL_URL`, `AUTOHEAL_AGENT_NAME`,
  `AUTOHEAL_SERVICE_USER`, `AUTOHEAL_INSTANCE` and `AUTOHEAL_YES`.
- Several projects on one server: the installer adds a further agent with its
  own config, state folder, credential and service (`--instance NAME`). All
  agents share the binary and the service user.
- A release workflow that runs on a `v*` tag: `make check`, shellcheck, the
  release build, a checksum and signature check, an end-to-end install test,
  and a GitHub release. A CI workflow runs the same checks and the install
  test on every push.
- `scripts/test-install.sh`, the end-to-end install test.
- Documentation pages under `docs/`, and this changelog.

### Changed

- `autoheal-agent validate` also says whether recovery is on.
- API errors in the log name the HTTP method as well as the path.

## 0.1.0 (2026-11-12)

The first version.

- `autoheal-agent enroll` trades a one-time token, read from
  `AUTOHEAL_ENROLLMENT_TOKEN` or standard input and never from an argument,
  for the agent's own credential. The credential and the API address are kept
  in an owner-only state folder.
- `autoheal-agent run` sends heartbeats, rotates the credential when AutoHeal
  asks, and picks up a credential replaced by `enroll --force` without a
  restart.
- CPU, memory and disk checks, and up to 50 configured HTTP and TCP checks. Each
  is reported as `<host_prefix>:<name>` once a status has held for two runs, and
  again every 5 minutes.
- A disk spool with count and size limits keeps events while AutoHeal is
  unreachable. Sends back off and honour `Retry-After`; event IDs make resends
  safe.
- Warnings when the agent is below AutoHeal's minimum version or the host's
  clock is off.
- `autoheal-agent validate` checks the config and reports every problem with
  its line number.
- A test that checks the source and the built binary for every release target
  for any way to start a process.
- A systemd unit, a launchd plist, an example config, and a reproducible
  release script that signs `SHA256SUMS` with minisign.
