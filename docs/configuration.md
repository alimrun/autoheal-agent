# Configuration

The agent reads one YAML file:

| | Path |
|---|---|
| Linux | `/etc/autoheal/agent.yaml` |
| macOS | `/usr/local/etc/autoheal/agent.yaml` |

Every command takes `--config PATH` to read another file. A further agent for
another project uses `<instance>.yaml` in the same folder
([Several projects on one server](installation.md#several-projects-on-one-server)).

[`packaging/agent.example.yaml`](../packaging/agent.example.yaml) is a commented
example. The installer copies it next to the config as `agent.example.yaml`.

- [Rules for the whole file](#rules-for-the-whole-file)
- [Reference](#reference)
- [How checks run and are reported](#how-checks-run-and-are-reported)
- [Apply a change](#apply-a-change)

## Rules for the whole file

- The config never holds the AutoHeal address or a secret. Those come from
  [enrollment](cli.md#enroll) and are kept in the state folder. A top-level
  `url`, `api_url` or `endpoint` key is refused.
- Unknown keys are refused, so a typo is an error, not a silent default.
- Durations are Go duration strings: `500ms`, `30s`, `5m`, `1h30m`.
- A key you leave out, or set to `0`, takes its default.
- `autoheal-agent validate` lists every problem at once, each as
  `<file>:<line>: <key>: <message>`, and changes nothing. `run` and `enroll`
  refuse to start with an invalid file.

## Reference

### Top level

| Key | Type | Default | Rule |
|---|---|---|---|
| `name` | string | none | At most 80 characters. The agent's name on the dashboard. It is sent only at enrollment; without it, AutoHeal keeps the name given to the enrollment token, or else the hostname. |
| `host_prefix` | string | the hostname, normalized | Lower-case letters, digits and `.` `_` `-`, 1 to 40 characters. Every check is reported as `<host_prefix>:<name>`, so two agents in one environment never share an incident. |
| `state_dir` | path | `/var/lib/autoheal-agent` (Linux), `/usr/local/var/autoheal-agent` (macOS) | An absolute path. Holds the credential, the spool and the recovery replay store. Must be readable only by the agent's user. |
| `ca_file` | path | none | A readable PEM file with at least one certificate. Its certificate authorities are trusted in addition to the system's own, for the AutoHeal address, HTTP checks and cache hooks behind an internal CA. |
| `log_level` | string | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON lines on standard error. |
| `spool` | map | | See [`spool`](#spool). |
| `host` | map | | See [`host`](#host). |
| `checks` | list | none | See [`checks`](#checks). |
| `recovery` | map | off | See [`recovery`](#recovery). |

The default `host_prefix` is the hostname in lower case, with every character
other than letters, digits and `.` `_` `-` replaced by `-`, runs of `-`
collapsed, `.` `_` `-` trimmed from both ends, and cut to 40 characters. An
empty result becomes `host`.

On Linux the shipped systemd unit lets the agent write only to
`/var/lib/autoheal-agent`. If you move `state_dir`, change `ReadWritePaths` and
`StateDirectory` in the unit too.

### `spool`

Events wait in the spool while AutoHeal cannot be reached. When it is full, the
oldest events are dropped.

| Key | Type | Default | Rule |
|---|---|---|---|
| `spool.max_events` | integer | `10000` | 50 to 1000000. |
| `spool.max_mb` | integer | `20` | 1 to 10240 (megabytes). |

### `host`

Host health: CPU, memory and disks, each up, degraded or down by percent used.

| Key | Type | Default | Rule |
|---|---|---|---|
| `host.enabled` | boolean | `true` | `false` turns off the CPU, memory and disk checks, and the other `host` keys are not checked. |
| `host.interval` | duration | `60s` | 10s to 1h. How often the host checks run. Each read times out after half the interval, at most 10s. |
| `host.cpu.degraded` | number | `85` | Above 0 and below `down`. |
| `host.cpu.down` | number | `95` | At most 100. |
| `host.memory.degraded` | number | `85` | Above 0 and below `down`. |
| `host.memory.down` | number | `95` | At most 100. |
| `host.disks` | list | one disk, `root` at `/` | Each entry is reported as `host.disk.<name>`. `disks: []` turns off the disk checks. |
| `host.disks[].name` | string | | Letters, digits and `.` `_` `-`, 1 to 49 characters, unique. |
| `host.disks[].path` | path | | An absolute path on the filesystem to measure. |
| `host.disks[].degraded` | number | `85` | Above 0 and below `down`. |
| `host.disks[].down` | number | `95` | At most 100. |

A reading at or above `down` is down, at or above `degraded` is degraded, and
anything lower is up. If the agent cannot read a value, the check is down.

### `checks`

HTTP and TCP checks, at most 50. Each entry sets exactly one of `http` or
`tcp`.

| Key | Type | Default | Rule |
|---|---|---|---|
| `checks[].name` | string | | Letters, digits and `.` `_` `-`, 1 to 59 characters, unique, and not starting with `host.`. |
| `checks[].http.url` | string | | A full `http://` or `https://` URL, without a user name or password. |
| `checks[].http.expect_status` | list of integers | any status from 200 to 399 | Each from 100 to 599. The statuses that count as up. |
| `checks[].tcp.address` | string | | `host:port`, with a port from 1 to 65535. Use `[::1]:5432` for an IPv6 address. |
| `checks[].interval` | duration | `60s` | 10s to 24h. |
| `checks[].timeout` | duration | `5s` | 1s to 60s, and shorter than `interval`. |
| `checks[].slow` | duration | off | Shorter than `timeout`. A check that succeeds but takes longer than this is degraded. |
| `checks[].down_severity` | string | `error` | `error` or `critical`. A down result is reported with this severity. `critical` makes AutoHeal investigate it automatically and email members. |

An HTTP check sends one `GET`. It is down when the request fails, times out, or
answers with a status not in `expect_status`; degraded when slower than `slow`;
up otherwise. It does not follow redirects, so a redirect status counts as the
answer (up under the default 200 to 399). It reads at most 64 KiB of the answer
and discards it. It verifies TLS certificates and uses no proxy.

A TCP check opens a connection and closes it. It is down when the connection
fails or times out, degraded when slower than `slow`, and up otherwise.

```yaml
checks:
  - name: api
    http:
      url: "http://127.0.0.1:8080/health"
      expect_status: [200]
    interval: 30s
    timeout: 5s
    slow: 1s

  - name: postgres
    tcp:
      address: "127.0.0.1:5432"
    interval: 30s
    timeout: 3s
    down_severity: critical
```

### `recovery`

Off by default. [Recovery](recovery.md) explains what it does and how to set up
the platform key.

| Key | Type | Default | Rule |
|---|---|---|---|
| `recovery.enabled` | boolean | `false` | `true` needs at least one platform key and at least one action with a target. |
| `recovery.platform_keys` | list | none | The public keys AutoHeal signs tasks with. List two while the server's key is rotated. |
| `recovery.platform_keys[].id` | string | | The key ID the server printed. Letters, digits and `.` `_` `-`, 1 to 64 characters, unique. |
| `recovery.platform_keys[].public_key` | string | | The public key the server printed: 32 bytes in standard base64. |
| `recovery.actions.recheck_health.checks` | list of strings | none | Checks AutoHeal may ask to run again, by their names in this file: a name from `checks`, or `host.cpu`, `host.memory`, `host.disk.<name>`. Each must be a check this agent runs, listed once. |
| `recovery.actions.clear_cache.hooks` | list | none | Cache hooks AutoHeal may ask to call. |
| `recovery.actions.clear_cache.hooks[].name` | string | | The cache name a task asks for. Starts with a letter or digit, then letters, digits and `.` `_` `:` `@` `-`, up to 100 characters, unique. |
| `recovery.actions.clear_cache.hooks[].url` | string | | A full `http://` or `https://` URL on this host: `localhost`, an address in `127.0.0.0/8`, or `::1`. No user name, password or fragment. |
| `recovery.actions.clear_cache.hooks[].token_file` | path | none | An absolute path to a regular file that exists and is not readable by group or other users. Its content, trimmed, is sent as `Authorization: Bearer <token>`. |
| `recovery.actions.clear_cache.hooks[].timeout` | duration | `30s` | 1s to 120s. |

An action with no targets is off. The keys and targets are checked even while
`enabled` is `false`, so a token file must exist and have the right mode as soon
as it is listed.

## How checks run and are reported

- Each check runs on its own interval. Its first run waits a random time of up
  to 10 seconds (at most one interval), so checks do not all start at once. Two
  runs of the same check never overlap.
- The CPU check measures use since its previous run, so its first run only
  takes a sample and reports nothing.
- A check is reported when a new status has held for two runs in a row, and
  then again every 5 minutes while it stays the same. A check that flips once
  and recovers is not reported.
- Up is reported with severity `info`, degraded with `warning`, and down with
  the check's `down_severity` (`error` for host checks).
- Each result carries the check name, the status, the kind (`cpu`, `memory`,
  `disk`, `http` or `tcp`), the target (a disk path, a URL reduced to scheme,
  host, port and path, or a `host:port`), the percentage or the latency, a short
  message, and the agent version.
- Results are written to the spool in batches of up to 50, or after 10 seconds,
  and sent from there. A failed send is retried with a growing wait of up to 5
  minutes, longer if AutoHeal asks for it. Events older than 23 hours are
  dropped, because AutoHeal accepts only the last 24 hours.

## Apply a change

The agent reads its config only when it starts. After an edit:

```bash
autoheal-agent validate
sudo systemctl restart autoheal-agent                    # Linux
sudo launchctl kickstart -k system/com.autoheal.agent    # macOS
```

For a further agent, validate with `--config` and restart its own service
(`autoheal-agent-<instance>`, or `com.autoheal.agent.<instance>` on macOS).
