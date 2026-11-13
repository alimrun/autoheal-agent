# Development

- [Build](#build)
- [make check](#make-check)
- [Layout](#layout)
- [The install test](#the-install-test)
- [Continuous integration](#continuous-integration)
- [Release](#release)
- [Sign releases](#sign-releases)

## Build

Requires Go (the version in `go.mod`).

```bash
make build      # ./autoheal-agent, version 0.0.0-dev unless HEAD is tagged
make test       # go test -race ./...
make check      # the gate before a commit or a release
make release V=0.2.0
```

`make build` builds with `CGO_ENABLED=0 -trimpath`. The version is stamped in
at build time from a tag on `HEAD` (`v0.2.0` gives `0.2.0`), or set it with
`make build VERSION=0.2.0`. The source keeps `0.0.0-dev` as the default, and
`COMMIT` and `DATE` default to the commit hash and time.

## make check

`make check` is the single gate before a commit or a release. It runs, in
order:

1. `gofmt -l .`, which fails on any file that needs formatting;
2. `go vet ./...`;
3. `go test -race ./...`;
4. `govulncheck ./...` if it is installed (`go install golang.org/x/vuln/cmd/govulncheck@latest`),
   or a note that it was skipped.

The tests include `internal/archtest`, which proves the agent cannot start a
process. It scans the module's Go files and fails on an `os/exec` import, on
`os.StartProcess`, `syscall.Exec`, `syscall.ForkExec` or
`syscall.StartProcess`, and on `InsecureSkipVerify` anywhere. It then builds
the agent for the four release targets and fails if the linked binary holds
`os.StartProcess`, `os/exec.(*Cmd).Start`, `syscall.forkExec` or
`syscall.Exec`. The golden files in `testdata/` pin the telemetry events and
the recovery task envelope.

## Layout

| Path | What it holds |
|---|---|
| `cmd/autoheal-agent` | The command line: `enroll`, `run`, `validate`, `version`. |
| `internal/config` | Loading, defaults and validation of `agent.yaml`. |
| `internal/api` | The client for the AutoHeal agent API. |
| `internal/credential` | The credential file and the state folder's modes. |
| `internal/runner` | Heartbeats, checks, batching, sending, rotation, recovery tasks and shutdown. |
| `internal/check` | The host, HTTP and TCP checks and when their results are reported. |
| `internal/event` | Telemetry events (schema version 1). |
| `internal/spool` | The on-disk queue of events not sent yet. |
| `internal/recovery` | Task verification, the replay store and the two actions. |
| `internal/backoff` | Retry waits. |
| `internal/buildinfo` | The version stamped in at build time. |
| `internal/archtest` | The no-process test. |
| `packaging/` | The example config, the systemd unit and the launchd plist. |
| `scripts/` | `install.sh`, `release.sh` and `test-install.sh`. |

## The install test

```bash
sudo scripts/test-install.sh dist
```

`scripts/test-install.sh <dist>` installs from the output of
`scripts/release.sh` on a Linux host with systemd, against a small stand-in for
the AutoHeal API on `127.0.0.1:18099`. It needs root, `python3` and `curl`, and
uses `dpkg`, so run it on a throwaway Debian or Ubuntu VM: it changes the host.
It checks that:

- an install without a terminal and without `--yes` is refused;
- a fresh install with a custom service user creates the user, the modes, the
  config, the credential and a running service, and prints the status;
- the running agent sends a heartbeat;
- running the installer again keeps the config and the credential and does not
  enroll again;
- a second agent for another project runs beside the first, and a different
  service user for it is refused;
- a tampered archive is refused and the binary is left alone;
- a token passed as an argument is refused, and the token is never printed.

## Continuous integration

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) runs on every push
and pull request:

1. `make check`, with `govulncheck` installed;
2. `shellcheck` on the three scripts;
3. an unsigned snapshot release (`scripts/release.sh 0.0.0-ci`, which must exit
   with status 3) and `sudo scripts/test-install.sh dist`.

## Release

`scripts/release.sh <version>` (or `make release V=<version>`) needs `go`,
`shasum`, a clean working tree and the tag `v<version>` on `HEAD`. The version
looks like `1.2.3` or `1.2.3-rc.1`, without a leading `v`. It:

1. builds `linux_amd64`, `linux_arm64`, `darwin_amd64` and `darwin_arm64` with
   `CGO_ENABLED=0 -trimpath`, into `dist/<os>_<arch>/autoheal-agent`;
2. packs `dist/autoheal-agent_<version>_<os>_<arch>.tar.gz`, each holding the
   binary, `README.md`, `CHANGELOG.md`, `docs/` and `packaging/` (and `LICENSE`
   and `minisign.pub` once they exist). The archives are reproducible: sorted entries owned by `0:0`,
   fixed modes, the commit time as every mtime, and gzip without a name or time;
3. writes `dist/SHA256SUMS`;
4. signs it into `dist/SHA256SUMS.minisig` with the minisign secret key file
   named by `MINISIGN_SECRET_KEY`, and checks the signature against
   `minisign.pub` if it is in the repository.

Without `MINISIGN_SECRET_KEY` it stops after the checksums, signs nothing, and
exits with status 3.

Pushing a tag `v<version>` runs
[`.github/workflows/release.yml`](../.github/workflows/release.yml): `make
check`, shellcheck, `scripts/release.sh`, a checksum (and signature) check, the
end-to-end install test, and then a GitHub release with the four archives,
`SHA256SUMS`, `SHA256SUMS.minisig` when signed, and `install.sh`. A version with
a suffix such as `0.3.0-rc.1` is published as a pre-release, which
`releases/latest` skips. The workflow fails if `minisign.pub` is committed but
the secret is missing.

```bash
git tag -a v0.2.0 -m 'autoheal-agent 0.2.0'
git push origin v0.2.0
```

Add a section to [CHANGELOG.md](../CHANGELOG.md) before you tag.

## Sign releases

Once:

1. Create a key without a password, so CI can use it:
   `minisign -G -W -p minisign.pub -s autoheal-release.key`.
2. Add the content of `autoheal-release.key` as the repository secret
   `MINISIGN_SECRET_KEY` (Settings, Secrets and variables, Actions). Keep an
   offline copy, and never commit it.
3. Commit `minisign.pub`. From the next tag, the workflow signs `SHA256SUMS`,
   fails if the secret is missing, and writes the public key into the released
   `install.sh`, which then refuses unsigned downloads.
