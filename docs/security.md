# Security

- [What the agent sends](#what-the-agent-sends)
- [What it never does](#what-it-never-does)
- [The credential](#the-credential)
- [Service sandbox](#service-sandbox)
- [Release signatures](#release-signatures)

## What the agent sends

Only to the AutoHeal API address that issued its credential:

- at enrollment: the agent's name (if the config sets one), hostname, version,
  operating system and architecture;
- on each heartbeat: the hostname, version, operating system and architecture;
  and, while recovery is on, the recovery protocol version and which of the two
  recovery actions are enabled (never their targets);
- check results: the check name and kind, up / degraded / down, the target
  (a sanitized URL, a host and port, or a disk path), the measured percentage
  or latency, a short error detail, and the agent version. URLs in results and
  logs are reduced to scheme, host, port and path; user information, query
  strings and fragments are dropped;
- for each recovery task: that it was received, started, and finished, failed
  or was refused, with a reason code, the duration, the hook's HTTP status or
  the check's status. Never the hook's address, its token or its answer.

It sends no file contents, process lists, environment variables or logs.

## What it never does

- **It never runs commands or starts a process.** Nothing in the binary can: a
  test builds every release target and fails if the linker kept any function
  that starts one (`os.StartProcess`, `os/exec`, `syscall.forkExec`,
  `syscall.Exec`), and a source scan forbids them and any way to skip TLS
  certificate checks. See [Development](development.md#make-check).
- **It takes no configuration from AutoHeal.** What it checks, and which hooks
  exist, is decided only by the local config file. Answers from AutoHeal set
  only the heartbeat interval (between 10 seconds and 1 hour), credential
  rotation and the minimum version notice.
- **With recovery off (the default) it takes no instructions at all** and
  never asks for tasks. With recovery on, the only instructions it accepts are
  signed tasks for the two actions in [Recovery](recovery.md), on targets its
  own config names; every other task is refused. A task cannot carry a
  command, an address or a check definition: unknown fields are refused.
- A cache hook is only ever called on a loopback address, without a proxy.
- It does not follow redirects and always verifies TLS certificates (TLS 1.2 or
  later; your system's CAs plus an optional `ca_file`), for the API, HTTP
  checks and cache hooks.
- It never logs the enrollment token, the credential or a hook token, and
  refuses a token passed as a command-line argument. The installer reads the
  token with echo off and passes it to the agent only through a pipe.

## The credential

`credential.json` lives in the state folder (`/var/lib/autoheal-agent` on
Linux, `/usr/local/var/autoheal-agent` on macOS), mode `0600` in a `0700`
folder owned by the service user. It is a bearer secret, stored in plain text
as any client's own secret must be; the file permissions protect it, and the
agent refuses to use it if they are loosened. It is only ever sent to the API
address saved next to it at enrollment, never to an address from the config
file. It is rotated automatically when AutoHeal asks, before it expires: the
agent saves the new credential before it uses it.

Revoke the agent on the dashboard if the server may be compromised, and before
you uninstall it.

## Service sandbox

The agent runs as its own unprivileged user, never as root; the installer
refuses root as the service user. The systemd unit adds:

| Setting | Effect |
|---|---|
| `NoNewPrivileges=yes` | The agent cannot gain privileges. |
| `ProtectSystem=strict` | The file system is read-only for it, except `ReadWritePaths=/var/lib/autoheal-agent`. |
| `ProtectHome=yes` | `/home`, `/root` and `/run/user` are hidden. |
| `PrivateTmp=yes` | It gets its own `/tmp`. |
| `CapabilityBoundingSet=` | No capabilities. |
| `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX` | IP and Unix sockets only. |

The launchd daemon runs as the service user with umask `077`.

## Release signatures

Every release carries `SHA256SUMS`, the SHA-256 checksums of its archives. The
release pipeline signs `SHA256SUMS` with [minisign](https://jedisct1.github.io/minisign/)
into `SHA256SUMS.minisig`, but the release key does not exist yet. **Until the
product owner creates it and commits only its public half as `minisign.pub` at
the root of this repository, releases carry checksums but no signature you can
check.**

Today the installer verifies the checksum and prints
`this release is not signed yet; verifying the checksum only`. Once
`minisign.pub` is in the repository, the release workflow writes its key into
the released `install.sh`, which then refuses a download with a missing or bad
signature. To check a download by hand, see
[Download and verify](installation.md#2-download-and-verify).
[Development](development.md#sign-releases) describes how to create the key.

The release archives are reproducible: the same commit, built on the same
platform, gives byte-identical archives.
