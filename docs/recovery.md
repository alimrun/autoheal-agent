# Recovery

From version 0.2.0 the agent can act on recovery tasks from AutoHeal. It is
**off unless the config turns it on**, and an agent with recovery off never
asks AutoHeal for tasks.

- [What it can do](#what-it-can-do)
- [Key setup](#key-setup)
- [Turn it on](#turn-it-on)
- [The cache hook contract](#the-cache-hook-contract)
- [How a task runs](#how-a-task-runs)
- [What the agent checks before acting](#what-the-agent-checks-before-acting)

## What it can do

With recovery on, the agent asks AutoHeal for a task every 15 seconds and
holds one at a time. It can do exactly two things, and only to targets named in
its own config:

| Action | What the agent does |
|---|---|
| `recheck_health` | Runs one of its configured checks once, now, and sends the result as ordinary telemetry. |
| `clear_cache` | Sends one HTTP `POST` to a cache hook you declared, on the same host. |

Anything else is refused. The agent does not take checks, hook addresses or
commands from AutoHeal, and it still cannot start a process.

## Key setup

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

## Turn it on

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
address that is not a loopback address, and a token file that is missing or
readable by group or other users. [Configuration](configuration.md#recovery)
lists every key and rule.

The agent's heartbeat tells AutoHeal which of the two actions are on, never
the check or hook names. Recovery also has to be turned on in AutoHeal's own
recovery policy, and the agent's credential needs the `recovery` scope. If
AutoHeal answers that the credential lacks it, the agent rotates its credential
once to get a new one with every scope; a credential issued before 0.2.0 gets
the scope this way.

## The cache hook contract

A hook is an HTTP endpoint **you** provide in your application; what it clears
is up to you. The agent's side of the contract:

- **Loopback only.** The URL's host must be `localhost`, an address in
  `127.0.0.0/8` or `::1`. The agent also checks the address it actually
  connects to, so a name that resolves elsewhere is refused. It uses no proxy.
- **One `POST`** per task, with `Content-Type: application/json` and the body
  `{"task_id": "<task ID>"}`. Nothing from AutoHeal other than that ID reaches
  the hook. The `User-Agent` is `autoheal-agent/<version> recovery`.
- **Optional bearer token.** With `token_file` set, the file's content
  (trimmed) is sent as `Authorization: Bearer <token>`. The file is read for
  each call, so you can change the token without a restart. It must be readable
  only by the agent's user:

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
  allows less. Connecting gives up after 5 seconds.
- **Make it safe to repeat.** A task runs at most once, but AutoHeal may send
  a new task for the same cache later.

Under the systemd unit, `/home`, `/root` and `/run/user` are hidden from the
agent (`ProtectHome`) and `/tmp` is private (`PrivateTmp`), so keep token files
elsewhere, such as `/etc/autoheal/hooks`.

## How a task runs

1. The agent verifies the task (below). A task that fails is refused, and
   AutoHeal is told why, unless the task is too damaged to name.
2. It records the task ID in `recovery-seen.json` in the state folder, then
   reports the task received.
3. It reports the task running, and acts only once AutoHeal accepts that.
4. It runs the action and reports it finished or failed, with how long it took
   and the hook's HTTP status or the check's status.
5. Once AutoHeal has accepted a finished report, the agent runs the health
   check named in the task and sends its result as telemetry. AutoHeal decides
   from that check whether the recovery worked. After a failed or refused task
   the check is not run.

AutoHeal does not take the hook's word, or the agent's, for the result. Each
report is retried for up to a minute while AutoHeal cannot be reached. If the
agent is stopped during an action, it makes one short attempt to report how
the action ended.

AutoHeal can pause recovery from the dashboard. While it is paused, the agent
keeps asking and acts on nothing. If the server has no recovery support yet,
the agent asks every 10 minutes instead of every 15 seconds.

A failed action has one of these reasons: `hook_status` (the hook answered
outside 2xx), `hook_error` (it could not be reached, or its token file could
not be read), `timeout`, or `check_error` (the check could not be run).

## What the agent checks before acting

Every task must pass all of these, in this order, or it is refused and nothing
is done. The refusal reason is in brackets.

1. It is signed by a key listed in `recovery.platform_keys` (`unknown_key`),
   and the signature is valid (`bad_signature`).
2. It uses protocol version 1, with no unknown fields (`unsupported_protocol`).
3. It has not expired (`expired`), was not issued more than 2 minutes ahead of
   this host's clock (`not_yet_valid`), and is valid for at most 10 minutes
   (`expired`).
4. It is addressed to this agent (`wrong_agent`) and its environment
   (`wrong_environment`).
5. Its task ID and nonce were never seen before (`replayed`). They are kept in
   `recovery-seen.json` across restarts, until an hour after the task expired.
6. Its action is one of the two (`unknown_action`) and is enabled in the local
   config (`not_enabled`).
7. Its arguments hold the target and nothing else (`invalid_arguments`).
8. Its target is declared in the local config (`not_enabled`): a hook name
   under `clear_cache`, or, for `recheck_health`, `<host_prefix>:<name>` with a
   name listed under `recheck_health`.

A second valid task that arrives while one is held is refused as `busy`.
