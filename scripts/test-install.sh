#!/usr/bin/env bash
# End-to-end test of scripts/install.sh on a Linux host with systemd, run by CI.
#
#   sudo scripts/test-install.sh <dist-dir>
#
# <dist-dir> is the output of scripts/release.sh. A small stand-in for the
# AutoHeal API answers enrollment, heartbeats and telemetry on 127.0.0.1. The
# test installs from the archives with a custom service user, checks the files,
# users and modes, re-runs the installer to prove it is idempotent, adds a
# second agent for another project, and checks that a tampered archive, a
# missing --yes without a terminal and a token argument are refused. It changes the host it runs on, so run it only on a throwaway VM.

set -euo pipefail

die() { echo "test-install: FAIL: $*" >&2; exit 1; }
pass() { echo "test-install: ok: $*"; }

[ $# -eq 1 ] || die "usage: sudo scripts/test-install.sh <dist-dir>"
[ "$(id -u)" -eq 0 ] || die "run as root on a throwaway Linux VM"
dist=$(cd "$1" && pwd -P)
root=$(cd "$(dirname "$0")/.." && pwd -P)
installer=$root/scripts/install.sh
work=$(mktemp -d)
port=18099
api=http://127.0.0.1:$port/api/v1
# A made-up value the stand-in API accepts; it must never appear in any output.
token=test-enrollment-token-$RANDOM$RANDOM

cleanup() {
	[ -n "${api_pid:-}" ] && kill "$api_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

cat >"$work/api.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

TOKEN = sys.argv[2]
REF = lambda name: {"id": "01J00000000000000000000000", "name": name}

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def answer(self, status, content):
        body = json.dumps({"content": content}).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        request = json.loads(self.rfile.read(length) or b"{}")
        path = self.path
        if path == "/api/v1/agent/register":
            if request.get("enrollment_token") != TOKEN:
                return self.answer(401, None)
            with open(sys.argv[3], "a") as seen:
                seen.write("register\n")
            return self.answer(201, {
                "agent": REF("ci-agent"), "project": REF("CI"), "environment": REF("test"),
                "credential": {"token": "ci-credential", "expires_at": "2099-01-01T00:00:00Z", "scopes": ["telemetry"]},
                "heartbeat_interval": 60, "minimum_version": "0.0.0",
            })
        if path == "/api/v1/agent/heartbeat":
            with open(sys.argv[3], "a") as seen:
                seen.write("heartbeat\n")
            return self.answer(200, {"heartbeat_interval": 60, "credential": {"expires_at": "2099-01-01T00:00:00Z", "rotate": False}, "minimum_version": "0.0.0"})
        if path == "/api/v1/agent/telemetry/events":
            return self.answer(202, {"accepted": len(request.get("events", [])), "duplicates": 0})
        return self.answer(404, None)

HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY
: >"$work/seen"
python3 "$work/api.py" "$port" "$token" "$work/seen" &
api_pid=$!
for _ in $(seq 50); do
	curl -s -o /dev/null "http://127.0.0.1:$port/" && break
	sleep 0.1
done

printf '%s\n' "$token" >"$work/token"
chmod 600 "$work/token"

run_installer() {
	local log=$1
	shift
	sh "$installer" --url "$api" --download-base "file://$dist" --token-stdin --yes "$@" <"$work/token" >"$log" 2>&1
}

user=autoheal-ci

# 0. Without a terminal and without --yes, nothing happens.
if sh "$installer" --url "$api" --download-base "file://$dist" --token-stdin <"$work/token" >"$work/no-yes.log" 2>&1; then
	die "an install without a terminal or --yes went ahead"
fi
grep -q "missing: --yes" "$work/no-yes.log" || { cat "$work/no-yes.log"; die "no message naming --yes"; }
[ ! -e /usr/local/bin/autoheal-agent ] || die "something was installed without --yes"
pass "no terminal and no --yes is refused"

# 1. A fresh install, with a service user of our choosing and a name.
run_installer "$work/first.log" --user "$user" --name ci-web || { cat "$work/first.log"; die "the first install failed"; }
cat "$work/first.log"
grep -qF "$token" "$work/first.log" && die "the installer printed the token"
id "$user" >/dev/null || die "no $user user"
[ "$(stat -c '%a %U' /usr/local/bin/autoheal-agent)" = "755 root" ] || die "binary mode or owner"
[ "$(stat -c '%a %U' /var/lib/autoheal-agent)" = "700 $user" ] || die "state folder mode or owner"
[ "$(stat -c '%a %U' /var/lib/autoheal-agent/credential.json)" = "600 $user" ] || die "credential mode or owner"
grep -q '^name: "ci-web"$' /etc/autoheal/agent.yaml || die "the config does not hold the name"
grep -q "^User=$user\$" /etc/systemd/system/autoheal-agent.service || die "the unit does not run as $user"
systemctl is-enabled --quiet autoheal-agent || die "service not enabled"
systemctl is-active --quiet autoheal-agent || die "service not active"
grep -q "Active: active" "$work/first.log" || die "the installer did not print the service status"
pass "fresh install: user, modes, config, credential, service, status"

for _ in $(seq 30); do
	grep -q heartbeat "$work/seen" && break
	sleep 1
done
grep -q heartbeat "$work/seen" || { journalctl -u autoheal-agent -n 50 --no-pager; die "no heartbeat reached the API"; }
pass "the running agent sent a heartbeat"

# 2. Running it again keeps the config and credential and does not enroll again.
echo "# kept" >>/etc/autoheal/agent.yaml
before=$(sha256sum /var/lib/autoheal-agent/credential.json)
run_installer "$work/second.log" || { cat "$work/second.log"; die "the second install failed"; }
grep -q "already enrolled" "$work/second.log" || die "the second run did not notice the credential"
grep -q "^# kept" /etc/autoheal/agent.yaml || die "the second run replaced the config"
[ "$(sha256sum /var/lib/autoheal-agent/credential.json)" = "$before" ] || die "the second run changed the credential"
[ "$(grep -c register "$work/seen")" -eq 1 ] || die "the second run enrolled again"
systemctl is-active --quiet autoheal-agent || die "service not active after the second run"
pass "re-run is idempotent"

# 2b. A second agent on the same server, for another project.
if run_installer "$work/other-user.log" --instance billing --user someone-else; then
	die "a second agent with another service user was accepted"
fi
run_installer "$work/billing.log" --instance billing --name ci-billing || { cat "$work/billing.log"; die "the second agent failed"; }
cat "$work/billing.log"
grep -qF "$token" "$work/billing.log" && die "the installer printed the token"
[ "$(stat -c '%a %U' /var/lib/autoheal-agent-billing)" = "700 $user" ] || die "second state folder mode or owner"
[ "$(stat -c '%a %U' /var/lib/autoheal-agent-billing/credential.json)" = "600 $user" ] || die "second credential mode or owner"
grep -q '^name: "ci-billing"$' /etc/autoheal/billing.yaml || die "the second config does not hold the name"
grep -q "run --config /etc/autoheal/billing.yaml" /etc/systemd/system/autoheal-agent-billing.service || die "the second unit does not use its config"
systemctl is-active --quiet autoheal-agent-billing || die "the second service is not active"
systemctl is-active --quiet autoheal-agent || die "the first service stopped"
[ "$(grep -c register "$work/seen")" -eq 2 ] || die "the second agent did not enroll exactly once"
grep -q "other agents on this server" "$work/billing.log" || die "the summary does not list the first agent"
pass "a second agent for another project runs beside the first"

# 3. A tampered archive is refused and nothing is replaced.
cp -R "$dist" "$work/tampered"
archive=$(cd "$work/tampered" && ls autoheal-agent_*_linux_"$(dpkg --print-architecture)".tar.gz)
printf 'x' >>"$work/tampered/$archive"
binary_before=$(sha256sum /usr/local/bin/autoheal-agent)
if sh "$installer" --url "$api" --download-base "file://$work/tampered" --token-stdin --yes <"$work/token" >"$work/tampered.log" 2>&1; then
	die "a tampered archive was installed"
fi
grep -q "checksum mismatch" "$work/tampered.log" || { cat "$work/tampered.log"; die "no checksum message"; }
[ "$(sha256sum /usr/local/bin/autoheal-agent)" = "$binary_before" ] || die "the binary changed after a refused install"
pass "a tampered archive is refused"

# 4. A token on the command line is refused before anything happens.
if sh "$installer" --url "$api" --token=abc >/dev/null 2>&1; then
	die "a token argument was accepted"
fi
pass "a token argument is refused"

echo "test-install: all passed"
