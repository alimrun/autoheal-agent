#!/bin/sh
# Install the AutoHeal agent on Linux (systemd) or macOS (launchd).
#
#   curl -fsSL https://github.com/alimrun/autoheal-agent/releases/latest/download/install.sh \
#     | sudo sh -s -- --url https://autoheal.example.com/api/v1
#
# It downloads the release for this platform, checks it against SHA256SUMS (and
# the minisign signature when the release is signed), installs the binary,
# user, folders, config and service as README sections 3 to 6 describe, asks
# for the enrollment token on the terminal, enrolls, and starts the service.
#
# Running it again is safe: it upgrades the binary, keeps the config and the
# credential, and restarts the service. The token is never taken from the
# command line or the environment, and never printed.
#
# Options:
#   --url URL            the API address from the dashboard, ending in /api/v1 (required)
#   --version X.Y.Z      install this version instead of the latest release
#   --reenroll           enroll again with a new token, replacing the credential
#   --token-stdin        read the token from standard input instead of the terminal;
#                        only when this script is a file, not piped from curl
#   --no-start           install and enroll, but do not enable or start the service
#   --download-base URL  fetch release files from URL/<file> (a mirror or a local test)

set -eu

REPO_URL=https://github.com/alimrun/autoheal-agent
SERVICE_USER=autoheal-agent
BIN=/usr/local/bin/autoheal-agent

# Filled in by the release workflow from minisign.pub. While it is empty,
# releases are unsigned and only the checksum is verified.
MINISIGN_PUBLIC_KEY=""

say() { printf 'autoheal: %s\n' "$*"; }
warn() { printf 'autoheal: %s\n' "$*" >&2; }
die() {
	printf 'autoheal: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
usage: install.sh --url https://<host>/api/v1 [options]

  --url URL            the API address from the dashboard, ending in /api/v1 (required)
  --version X.Y.Z      install this version instead of the latest release
  --reenroll           enroll again with a new token, replacing the credential
  --token-stdin        read the token from standard input instead of the terminal
                       (only when the script is a file, not piped from curl)
  --no-start           install and enroll, but do not enable or start the service
  --download-base URL  fetch release files from URL/<file> (a mirror or a local test)
EOF
}

# Everything runs from main, so a shell reading this script from a pipe has
# parsed all of it before the first command runs, and no command can swallow
# the rest of the script from standard input.
main() {
	api_url=""
	version=""
	reenroll=0
	token_stdin=0
	start_service=1
	download_base=""

	while [ $# -gt 0 ]; do
		case $1 in
		--url) [ $# -ge 2 ] || die "--url needs a value"; api_url=$2; shift 2 ;;
		--url=*) api_url=${1#--url=}; shift ;;
		--version) [ $# -ge 2 ] || die "--version needs a value"; version=$2; shift 2 ;;
		--version=*) version=${1#--version=}; shift ;;
		--download-base) [ $# -ge 2 ] || die "--download-base needs a value"; download_base=$2; shift 2 ;;
		--download-base=*) download_base=${1#--download-base=}; shift ;;
		--reenroll) reenroll=1; shift ;;
		--token-stdin) token_stdin=1; shift ;;
		--no-start) start_service=0; shift ;;
		-h | --help) usage; exit 0 ;;
		--token | --token=* | --enrollment-token | --enrollment-token=*)
			die "the enrollment token is never passed as an argument, where other users can see it; the script asks for it" ;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
	done

	# --- Checks before anything changes ------------------------------------------

	[ -n "$api_url" ] || die "--url is required: the API address on the dashboard's Agents page, ending in /api/v1"
	case $api_url in
	https://*) ;;
	http://localhost | http://localhost[:/]* | http://127.0.0.1 | http://127.0.0.1[:/]* | http://\[::1\] | http://\[::1\][:/]*) ;;
	*) die "--url must start with https:// (http:// is allowed only for localhost, 127.0.0.1 or [::1])" ;;
	esac
	case $api_url in
	*[!A-Za-z0-9:/._~%@\[\]-]*) die "--url holds characters an API address does not use" ;;
	esac
	api_url=${api_url%/}

	case $download_base in
	"" | https://* | file:///*) ;;
	http://localhost[:/]* | http://127.0.0.1[:/]*) ;;
	*) die "--download-base must be https://, file:// or http:// to localhost" ;;
	esac

	if [ -n "$version" ]; then
		version=${version#v}
		case $version in
		[0-9]*.[0-9]*.[0-9]*) ;;
		*) die "--version must look like 1.2.3 (got '$version')" ;;
		esac
		case $version in
		*[!0-9A-Za-z.-]*) die "--version must look like 1.2.3 (got '$version')" ;;
		esac
	fi

	[ "$(id -u)" -eq 0 ] || die "run this as root, for example: curl -fsSL … | sudo sh -s -- --url …"

	case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported operating system $(uname -s); the agent runs on Linux and macOS" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m); the agent runs on amd64 and arm64" ;;
	esac

	if [ "$os" = linux ]; then
		config_dir=/etc/autoheal
		state_dir=/var/lib/autoheal-agent
		nologin=/usr/sbin/nologin
		[ -x "$nologin" ] || nologin=/sbin/nologin
		if [ "$start_service" -eq 1 ] && ! { command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; }; then
			die "systemd is not running here; install the service yourself (README section 6) and run again with --no-start"
		fi
	else
		config_dir=/usr/local/etc/autoheal
		state_dir=/usr/local/var/autoheal-agent
		log_file=/usr/local/var/log/autoheal-agent.log
		plist=/Library/LaunchDaemons/com.autoheal.agent.plist
	fi
	config_file=$config_dir/agent.yaml
	credential_file=$state_dir/credential.json

	if command -v curl >/dev/null 2>&1; then
		fetch() { curl -fsSL --proto '=https,http,file' --proto-redir '=https' --retry 3 -o "$2" "$1"; }
	elif command -v wget >/dev/null 2>&1; then
		fetch() { wget -q -O "$2" "$1"; }
	else
		die "curl or wget is needed to download the agent"
	fi

	if command -v sha256sum >/dev/null 2>&1; then
		sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
	elif command -v shasum >/dev/null 2>&1; then
		sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
	else
		die "sha256sum or shasum is needed to verify the download"
	fi

	if [ -n "$MINISIGN_PUBLIC_KEY" ] && ! command -v minisign >/dev/null 2>&1; then
		die "releases are signed, and minisign is needed to check the signature: install it (apt install minisign, dnf install minisign, brew install minisign) and run again"
	fi

	work=$(mktemp -d "${TMPDIR:-/tmp}/autoheal-install.XXXXXX")
	token=""
	restore_tty() { :; }
	cleanup() {
		restore_tty
		token=""
		rm -rf "$work"
	}
	trap cleanup EXIT
	trap 'exit 130' INT TERM

	# --- Download and verify -----------------------------------------------------

	if [ -n "$download_base" ]; then
		base=${download_base%/}
	elif [ -n "$version" ]; then
		base=$REPO_URL/releases/download/v$version
	else
		base=$REPO_URL/releases/latest/download
	fi

	say "downloading the checksums from $base"
	fetch "$base/SHA256SUMS" "$work/SHA256SUMS" || die "could not download $base/SHA256SUMS"

	line=$(grep -E "^[0-9a-f]{64}  autoheal-agent_[0-9A-Za-z.-]+_${os}_${arch}\.tar\.gz\$" "$work/SHA256SUMS" | head -n 1 || true)
	[ -n "$line" ] || die "SHA256SUMS lists no archive for ${os}_${arch}"
	expected=${line%% *}
	archive=${line##* }
	found_version=${archive#autoheal-agent_}
	found_version=${found_version%_"${os}"_"${arch}".tar.gz}
	if [ -n "$version" ] && [ "$found_version" != "$version" ]; then
		die "asked for $version, but the release lists $found_version"
	fi
	version=$found_version
	# Pin the rest of the download to that version, so a release published in
	# between cannot mix files from two versions.
	if [ -z "$download_base" ]; then
		base=$REPO_URL/releases/download/v$version
	fi

	if [ -n "$MINISIGN_PUBLIC_KEY" ]; then
		fetch "$base/SHA256SUMS.minisig" "$work/SHA256SUMS.minisig" ||
			die "this release has no SHA256SUMS.minisig; refusing an unsigned download"
		minisign -Vm "$work/SHA256SUMS" -P "$MINISIGN_PUBLIC_KEY" >/dev/null 2>&1 ||
			die "the signature on SHA256SUMS does not verify; refusing to install"
		say "signature verified"
	else
		warn "this release is not signed yet; verifying the checksum only"
	fi

	say "downloading $archive"
	fetch "$base/$archive" "$work/$archive" || die "could not download $base/$archive"
	actual=$(sha256 "$work/$archive")
	[ "$actual" = "$expected" ] || die "checksum mismatch for $archive (expected $expected, got $actual); refusing to install"
	say "checksum verified"

	mkdir "$work/unpacked"
	tar -xzf "$work/$archive" -C "$work/unpacked"
	src=$work/unpacked/${archive%.tar.gz}
	[ -f "$src/autoheal-agent" ] || die "the archive does not hold autoheal-agent"

	# --- Service user ------------------------------------------------------------

	free_darwin_id() {
		used=$(
			dscl . -list /Users UniqueID | awk '{print $2}'
			dscl . -list /Groups PrimaryGroupID | awk '{print $2}'
		)
		candidate=499
		while [ "$candidate" -ge 400 ]; do
			if ! printf '%s\n' "$used" | grep -qx "$candidate"; then
				echo "$candidate"
				return 0
			fi
			candidate=$((candidate - 1))
		done
		return 1
	}

	if [ "$os" = linux ]; then
		if ! getent group "$SERVICE_USER" >/dev/null 2>&1 && ! id "$SERVICE_USER" >/dev/null 2>&1; then
			groupadd --system "$SERVICE_USER"
		fi
		if ! id "$SERVICE_USER" >/dev/null 2>&1; then
			say "creating the system user $SERVICE_USER"
			useradd --system --gid "$SERVICE_USER" --no-create-home --home-dir "$state_dir" --shell "$nologin" "$SERVICE_USER"
		fi
	else
		group_id=$(dscl . -read "/Groups/$SERVICE_USER" PrimaryGroupID 2>/dev/null | awk '{print $2}' || true)
		if [ -z "$group_id" ]; then
			group_id=$(free_darwin_id) || die "no free ID between 400 and 499 for the $SERVICE_USER group"
			dscl . -create "/Groups/$SERVICE_USER" PrimaryGroupID "$group_id"
		fi
		if ! dscl . -read "/Users/$SERVICE_USER" UniqueID >/dev/null 2>&1; then
			say "creating the system user $SERVICE_USER"
			if dscl . -list /Users UniqueID | awk '{print $2}' | grep -qx "$group_id"; then
				user_id=$(free_darwin_id) || die "no free ID between 400 and 499 for the $SERVICE_USER user"
			else
				user_id=$group_id
			fi
			dscl . -create "/Users/$SERVICE_USER" UniqueID "$user_id"
			dscl . -create "/Users/$SERVICE_USER" PrimaryGroupID "$group_id"
			dscl . -create "/Users/$SERVICE_USER" UserShell /usr/bin/false
			dscl . -create "/Users/$SERVICE_USER" NFSHomeDirectory /var/empty
			dscl . -create "/Users/$SERVICE_USER" IsHidden 1
		fi
	fi

	# --- Binary, folders and config ----------------------------------------------

	say "installing autoheal-agent $version to $BIN"
	install -d -m 0755 /usr/local/bin
	install -m 0755 "$src/autoheal-agent" "$BIN.new"
	mv -f "$BIN.new" "$BIN"

	install -d -m 0755 "$config_dir"
	install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$state_dir"
	# A folder or credential loosened by hand would make the agent refuse to start.
	chown "$SERVICE_USER:$SERVICE_USER" "$state_dir"
	chmod 0700 "$state_dir"
	if [ -f "$credential_file" ]; then
		chown "$SERVICE_USER:$SERVICE_USER" "$credential_file"
		chmod 0600 "$credential_file"
	fi

	if [ "$os" = darwin ]; then
		install -d -m 0755 /usr/local/var/log
		[ -f "$log_file" ] || : >"$log_file"
		chown "$SERVICE_USER:$SERVICE_USER" "$log_file"
		chmod 0600 "$log_file"
	fi

	install -m 0644 "$src/packaging/agent.example.yaml" "$config_dir/agent.example.yaml"
	if [ -f "$config_file" ]; then
		say "keeping the existing config $config_file"
	else
		say "writing a starter config to $config_file (host checks only)"
		cat >"$config_file.new" <<'EOF'
# AutoHeal agent configuration, written by install.sh.
#
# It reports CPU, memory and root disk use. Add HTTP and TCP checks for your
# services under `checks:`; agent.example.yaml next to this file describes
# every setting. After editing, run `autoheal-agent validate` and restart the
# service.

log_level: info

host:
  interval: 60s
  cpu: { degraded: 85, down: 95 }
  memory: { degraded: 85, down: 95 }
  disks:
    - { name: root, path: /, degraded: 85, down: 95 }

checks: []
EOF
		chmod 0644 "$config_file.new"
		mv -f "$config_file.new" "$config_file"
	fi
	"$BIN" validate --config "$config_file" || die "the config $config_file is not valid; fix it and run the script again"

	# --- Enroll ------------------------------------------------------------------

	as_service_user() {
		if [ "$os" = linux ] && command -v runuser >/dev/null 2>&1; then
			runuser -u "$SERVICE_USER" -- "$@"
		elif command -v sudo >/dev/null 2>&1; then
			sudo -u "$SERVICE_USER" -- "$@"
		else
			die "runuser or sudo is needed to enroll as $SERVICE_USER"
		fi
	}

	read_token() {
		if [ "$token_stdin" -eq 1 ]; then
			IFS= read -r token || true
			return 0
		fi
		if ! { : </dev/tty; } 2>/dev/null; then
			die "no terminal to ask for the enrollment token on; run the script from a file with --token-stdin"
		fi
		printf 'Paste the enrollment token from the dashboard (it is not shown): ' >/dev/tty
		if stty -echo </dev/tty 2>/dev/null; then
			restore_tty() { stty echo </dev/tty 2>/dev/null || true; }
		fi
		IFS= read -r token </dev/tty || true
		restore_tty
		restore_tty() { :; }
		printf '\n' >/dev/tty
	}

	enrolled=0
	if [ -f "$credential_file" ] && [ "$reenroll" -eq 0 ]; then
		say "already enrolled ($credential_file exists); keeping that credential"
		if ! tr -d ' \n' <"$credential_file" | grep -qF "\"api_url\":\"$api_url\""; then
			warn "the saved credential may be for another API address; run again with --reenroll and a new token to switch"
		fi
	else
		attempts=0
		while [ "$enrolled" -eq 0 ]; do
			attempts=$((attempts + 1))
			read_token
			token=$(printf '%s' "$token" | tr -d ' \t\r\n')
			if [ -z "$token" ]; then
				warn "no token entered"
			else
				# A clean environment: the URL and proxy settings, nothing else of root's.
				set -- env -i "AUTOHEAL_URL=$api_url"
				if [ -n "${HTTPS_PROXY:-}" ]; then set -- "$@" "HTTPS_PROXY=$HTTPS_PROXY"; fi
				if [ -n "${https_proxy:-}" ]; then set -- "$@" "https_proxy=$https_proxy"; fi
				if [ -n "${NO_PROXY:-}" ]; then set -- "$@" "NO_PROXY=$NO_PROXY"; fi
				if [ -n "${no_proxy:-}" ]; then set -- "$@" "no_proxy=$no_proxy"; fi
				set -- "$@" "$BIN" enroll --token-stdin --config "$config_file"
				if [ "$reenroll" -eq 1 ]; then set -- "$@" --force; fi
				# The token travels only through this pipe: printf is a shell builtin,
				# so it is never in a process's arguments or environment.
				if (cd / && printf '%s\n' "$token" | as_service_user "$@"); then
					enrolled=1
				fi
			fi
			token=""
			if [ "$enrolled" -eq 0 ]; then
				if [ "$token_stdin" -eq 1 ] || [ "$attempts" -ge 3 ]; then
					die "enrollment failed; create a new token on the dashboard and run the script again"
				fi
				warn "try again with a token from the dashboard (attempt $((attempts + 1)) of 3)"
			fi
		done
	fi

	# --- Service -----------------------------------------------------------------

	if [ "$start_service" -eq 0 ]; then
		say "installed and enrolled; not starting the service (--no-start)"
		exit 0
	fi

	if [ "$os" = linux ]; then
		install -m 0644 "$src/packaging/autoheal-agent.service" /etc/systemd/system/autoheal-agent.service
		systemctl daemon-reload
		systemctl enable autoheal-agent >/dev/null 2>&1
		systemctl restart autoheal-agent
		sleep 2
		systemctl is-active --quiet autoheal-agent ||
			die "the service did not start; see: journalctl -u autoheal-agent -n 50"
		say "autoheal-agent $version is running. Logs: journalctl -u autoheal-agent -f"
	else
		install -m 0644 -o root -g wheel "$src/packaging/com.autoheal.agent.plist" "$plist"
		launchctl bootout system/com.autoheal.agent >/dev/null 2>&1 || true
		launchctl bootstrap system "$plist" || die "launchctl could not load $plist"
		sleep 2
		launchctl print system/com.autoheal.agent 2>/dev/null | grep -q 'state = running' ||
			die "the service did not start; see: tail -n 50 $log_file"
		say "autoheal-agent $version is running. Logs: tail -f $log_file"
	fi
	say "within a minute it shows as online on the environment's Agents page"
}

main "$@"
