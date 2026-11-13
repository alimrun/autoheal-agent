#!/bin/sh
# Install the AutoHeal agent on Linux (systemd) or macOS (launchd).
#
#   curl -fsSL https://github.com/alimrun/autoheal-agent/releases/latest/download/install.sh | sudo sh
#
# It asks on the terminal for anything not given as an option: the API address,
# the enrollment token (with echo off), the agent's name and the service user.
# It checks the system and its tools, downloads the release for this platform,
# checks it against SHA256SUMS (and the minisign signature when the release is
# signed), shows what it will do and asks before changing anything. It then
# installs the binary, user, folders, config and service as the manual steps
# in docs/installation.md describe, enrolls, starts the service and prints its status.
#
# One server can report to several projects: run it again with the token of
# another project's environment and it adds a second agent beside the first,
# with its own config, credential and service (--instance NAME).
#
# Running it again is safe: it upgrades the binary, keeps the config and the
# credential, and restarts the services. The token is never taken from the
# command line or the environment, and never printed.
#
# Without a terminal (automation), pass --url, --token-stdin and --yes.

set -eu

REPO_URL=https://github.com/alimrun/autoheal-agent
DEFAULT_SERVICE_USER=autoheal-agent
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
usage: install.sh [options]

Asks on the terminal for anything not given here.

  --url URL            the API address from the dashboard, ending in /api/v1
                       (or set AUTOHEAL_URL)
  --name NAME          the agent's name on the dashboard; defaults to the host name
                       (or set AUTOHEAL_AGENT_NAME)
  --user USER          the system user the service runs as; defaults to autoheal-agent
                       (or set AUTOHEAL_SERVICE_USER)
  --instance NAME      a further agent on this server, for another project; it gets
                       its own config, credential and service (or set AUTOHEAL_INSTANCE)
  -y, --yes            do not ask to confirm, and take the defaults for the name
                       and the user (or set AUTOHEAL_YES=1)
  --token-stdin        read the token from standard input instead of the terminal
                       (only when the script is a file, not piped from curl)
  --reenroll           enroll again with a new token, replacing the credential
  --version X.Y.Z      install this version instead of the latest release
  --no-start           install and enroll, but do not enable or start the service
  --download-base URL  fetch release files from URL/<file> (a mirror or a local test)

Without a terminal, --url, --token-stdin and --yes are required. The token is
never accepted as an option or an environment variable.
EOF
}

# Prints why an API address is unusable, or nothing when it is fine.
url_problem() {
	case $1 in
	"") echo "the API address is empty"; return 0 ;;
	https://*) ;;
	http://localhost | http://localhost[:/]* | http://127.0.0.1 | http://127.0.0.1[:/]* | http://\[::1\] | http://\[::1\][:/]*) ;;
	*) echo "the API address must start with https:// (http:// is allowed only for localhost, 127.0.0.1 or [::1])"; return 0 ;;
	esac
	case $1 in
	*[!A-Za-z0-9:/._~%@\[\]-]*) echo "the API address holds characters an address does not use" ;;
	esac
}

# Prints why a service user name is unusable, or nothing when it is fine.
user_problem() {
	case $1 in
	root) echo "the agent never runs as root; choose another user"; return 0 ;;
	"" | [!a-z_]* | *[!a-z0-9_-]*) echo "a user name is lower-case letters, digits, '_' and '-', starting with a letter or '_'"; return 0 ;;
	esac
	if [ "${#1}" -gt 32 ]; then
		echo "a user name is at most 32 characters"
		return 0
	fi
	if id -u "$1" >/dev/null 2>&1 && [ "$(id -u "$1")" -eq 0 ]; then
		echo "$1 has user ID 0, which is root"
	fi
}

# Prints why an instance name is unusable, or nothing when it is fine.
instance_problem() {
	case $1 in
	"" | [!a-z0-9]* | *[!a-z0-9-]*) echo "an instance name is lower-case letters, digits and '-', starting with a letter or digit"; return 0 ;;
	agent | agent.example) echo "'$1' is taken by the agent's own files; choose another name"; return 0 ;;
	esac
	if [ "${#1}" -gt 30 ]; then
		echo "an instance name is at most 30 characters"
	fi
}

# Prints why an agent name is unusable, or nothing when it is fine. The agent
# itself checks the length (at most 80 characters) when the config is validated.
name_problem() {
	case $1 in
	"") echo "the name is empty" ;;
	*[[:cntrl:]\"\\]*) echo "the name may not hold quotes, backslashes or control characters" ;;
	esac
}

# Everything runs from main, so a shell reading this script from a pipe has
# parsed all of it before the first command runs, and no command can swallow
# the rest of the script from standard input.
main() {
	api_url=${AUTOHEAL_URL:-}
	agent_name=${AUTOHEAL_AGENT_NAME:-}
	service_user=${AUTOHEAL_SERVICE_USER:-}
	instance=${AUTOHEAL_INSTANCE:-}
	assume_yes=0
	case ${AUTOHEAL_YES:-} in 1 | true | yes) assume_yes=1 ;; esac
	version=""
	reenroll=0
	token_stdin=0
	start_service=1
	download_base=""

	while [ $# -gt 0 ]; do
		case $1 in
		--url) [ $# -ge 2 ] || die "--url needs a value"; api_url=$2; shift 2 ;;
		--url=*) api_url=${1#--url=}; shift ;;
		--name) [ $# -ge 2 ] || die "--name needs a value"; agent_name=$2; shift 2 ;;
		--name=*) agent_name=${1#--name=}; shift ;;
		--user) [ $# -ge 2 ] || die "--user needs a value"; service_user=$2; shift 2 ;;
		--user=*) service_user=${1#--user=}; shift ;;
		--instance) [ $# -ge 2 ] || die "--instance needs a value"; instance=$2; shift 2 ;;
		--instance=*) instance=${1#--instance=}; shift ;;
		--version) [ $# -ge 2 ] || die "--version needs a value"; version=$2; shift 2 ;;
		--version=*) version=${1#--version=}; shift ;;
		--download-base) [ $# -ge 2 ] || die "--download-base needs a value"; download_base=$2; shift 2 ;;
		--download-base=*) download_base=${1#--download-base=}; shift ;;
		-y | --yes) assume_yes=1; shift ;;
		--reenroll) reenroll=1; shift ;;
		--token-stdin) token_stdin=1; shift ;;
		--no-start) start_service=0; shift ;;
		-h | --help) usage; exit 0 ;;
		--token | --token=* | --enrollment-token | --enrollment-token=*)
			die "the enrollment token is never passed as an argument, where other users can see it; the script asks for it" ;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
	done
	if [ -n "${AUTOHEAL_ENROLLMENT_TOKEN:-}" ]; then
		warn "ignoring AUTOHEAL_ENROLLMENT_TOKEN: the script asks for the token, or reads it with --token-stdin"
	fi

	# --- Checks on the options -----------------------------------------------------

	if [ -n "$api_url" ]; then
		problem=$(url_problem "$api_url")
		[ -z "$problem" ] || die "--url: $problem"
	fi
	if [ -n "$service_user" ]; then
		problem=$(user_problem "$service_user")
		[ -z "$problem" ] || die "--user: $problem"
	fi
	if [ -n "$agent_name" ]; then
		problem=$(name_problem "$agent_name")
		[ -z "$problem" ] || die "--name: $problem"
	fi
	instance_given=0
	if [ -n "$instance" ]; then
		instance_given=1
		problem=$(instance_problem "$instance")
		[ -z "$problem" ] || die "--instance: $problem"
	fi

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

	[ "$(id -u)" -eq 0 ] || die "run this as root, for example: curl -fsSL … | sudo sh"

	# --- The system and its tools --------------------------------------------------

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

	family=""
	if [ "$os" = linux ]; then
		system_name=Linux
		ids=""
		if [ -r /etc/os-release ]; then
			# shellcheck disable=SC1091
			system_name=$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-${NAME:-Linux}}")
			# shellcheck disable=SC1091
			ids=$(. /etc/os-release && printf '%s %s' "${ID:-}" "${ID_LIKE:-}")
		fi
		case " $ids " in
		*" debian "* | *" ubuntu "*) family=debian ;;
		*" rhel "* | *" fedora "* | *" centos "* | *" amzn "*) family=rhel ;;
		*" suse "* | *" sles "* | *" opensuse "*) family=suse ;;
		*" alpine "*) family=alpine ;;
		*" arch "*) family=arch ;;
		esac
		config_dir=/etc/autoheal
		state_base=/var/lib/autoheal-agent
		nologin=/usr/sbin/nologin
		[ -x "$nologin" ] || nologin=/sbin/nologin
	else
		system_name="macOS $(sw_vers -productVersion 2>/dev/null || true)"
		config_dir=/usr/local/etc/autoheal
		state_base=/usr/local/var/autoheal-agent
		log_base=/usr/local/var/log/autoheal-agent
	fi

	# Each agent on this server is an instance. The first, "default", keeps the
	# paths docs/installation.md describes; another project's agent, NAME, uses
	# NAME.yaml, its own state folder and its own service.
	suffix_for() { if [ "$1" != default ]; then echo "-$1"; fi; }
	config_for() { if [ "$1" = default ]; then echo "$config_dir/agent.yaml"; else echo "$config_dir/$1.yaml"; fi; }
	state_for() { echo "$state_base$(suffix_for "$1")"; }
	unit_for() { echo "autoheal-agent$(suffix_for "$1")"; }
	label_for() { if [ "$1" = default ]; then echo com.autoheal.agent; else echo "com.autoheal.agent.$1"; fi; }
	set_instance() {
		instance=$1
		config_file=$(config_for "$instance")
		state_dir=$(state_for "$instance")
		credential_file=$state_dir/credential.json
		unit_name=$(unit_for "$instance")
		unit_file=/etc/systemd/system/$unit_name.service
		label=$(label_for "$instance")
		plist=/Library/LaunchDaemons/$label.plist
		log_file=${log_base:-}$(suffix_for "$instance").log
		need_token=1
		if [ -f "$credential_file" ] && [ "$reenroll" -eq 0 ]; then
			need_token=0
		fi
		have_config=0
		if [ -f "$config_file" ]; then have_config=1; fi
	}

	# The instances already installed here, one name per line.
	list_instances() {
		for listed in "$config_dir"/*.yaml; do
			[ -f "$listed" ] || continue
			listed=${listed##*/}
			listed=${listed%.yaml}
			case $listed in agent) listed=default ;; agent.example) continue ;; esac
			echo "$listed"
		done
	}

	# ref_name FILE KEY: the name of the agent, project or environment in a
	# credential. Only names are read from it, never its token.
	ref_name() {
		tr -d '\n' <"$1" | sed -n "s/.*\"$2\": *{[^}]*\"name\": *\"\\([^\"]*\\)\".*/\\1/p"
	}
	describe_instance() {
		described=$(state_for "$1")/credential.json
		if [ -f "$described" ]; then
			printf '%s in %s · %s\n' "$(ref_name "$described" agent)" "$(ref_name "$described" project)" "$(ref_name "$described" environment)"
		else
			echo "installed, not enrolled"
		fi
	}

	# The package that holds a command on this distribution.
	package_for() {
		case $1 in
		useradd | groupadd)
			case $family in debian) echo passwd ;; rhel) echo shadow-utils ;; *) echo shadow ;; esac ;;
		runuser) echo util-linux ;;
		sha256sum | install | mktemp) echo coreutils ;;
		*) echo "$1" ;;
		esac
	}
	missing=""
	packages=""
	need() {
		missing="$missing $1"
		package=$(package_for "$1")
		case " $packages " in *" $package "*) ;; *) packages="$packages $package" ;; esac
	}
	have() { command -v "$1" >/dev/null 2>&1; }

	have curl || have wget || need curl
	have sha256sum || have shasum || need sha256sum
	for tool in tar mktemp install; do have "$tool" || need "$tool"; done
	if [ "$os" = linux ]; then
		have gzip || need gzip
		have useradd || need useradd
		have groupadd || need groupadd
		have runuser || have sudo || need runuser
	fi
	[ -z "$MINISIGN_PUBLIC_KEY" ] || have minisign || need minisign
	if [ -n "$missing" ]; then
		case $family in
		debian) hint="apt-get install -y$packages" ;;
		rhel) if have dnf; then hint="dnf install -y$packages"; else hint="yum install -y$packages"; fi ;;
		suse) hint="zypper install -y$packages" ;;
		alpine) hint="apk add$packages" ;;
		arch) hint="pacman -S --noconfirm$packages" ;;
		*) hint="" ;;
		esac
		if [ "$os" = darwin ]; then hint="brew install$packages"; fi
		if [ -n "$hint" ]; then
			die "missing tools:$missing. Install them with: $hint (minisign may need EPEL on RHEL), then run this again"
		fi
		die "missing tools:$missing. Install the packages$packages, then run this again"
	fi
	if [ "$os" = linux ] && [ "$start_service" -eq 1 ] && ! { have systemctl && [ -d /run/systemd/system ]; }; then
		die "systemd is not running here; install the service yourself (docs/installation.md, "6. Run as a service") and run again with --no-start"
	fi

	if have curl; then
		fetch() { curl -fsSL --proto '=https,http,file' --proto-redir '=https' --retry 3 -o "$2" "$1"; }
	else
		fetch() { wget -q -O "$2" "$1"; }
	fi
	if have sha256sum; then
		sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
	else
		sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
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

	# --- What is already here ------------------------------------------------------

	# All agents on a server share the binary and the service user.
	previous_user=""
	if [ "$os" = linux ]; then
		for file in /etc/systemd/system/autoheal-agent.service /etc/systemd/system/autoheal-agent-*.service; do
			[ -f "$file" ] || continue
			previous_user=$(sed -n 's/^User=//p' "$file" | head -n 1)
			[ -z "$previous_user" ] || break
		done
	else
		for file in /Library/LaunchDaemons/com.autoheal.agent.plist /Library/LaunchDaemons/com.autoheal.agent.*.plist; do
			[ -f "$file" ] || continue
			previous_user=$(/usr/libexec/PlistBuddy -c 'Print :UserName' "$file" 2>/dev/null || true)
			[ -z "$previous_user" ] || break
		done
	fi
	existing_instances=$(list_instances)
	if [ -z "$previous_user" ]; then
		# Installed with --no-start: the owner of an agent's state folder.
		for name in $existing_instances; do
			dir=$(state_for "$name")
			if [ -d "$dir" ]; then
				if [ "$os" = linux ]; then previous_user=$(stat -c %U "$dir"); else previous_user=$(stat -f %Su "$dir"); fi
				break
			fi
		done
	fi
	installed_version=""
	if [ -x "$BIN" ]; then
		installed_version=$("$BIN" version 2>/dev/null | awk 'NR == 1 { print $2 }' || true)
	fi
	set_instance "${instance:-default}"

	# --- Inputs ----------------------------------------------------------------------

	# In a subshell: dash exits the whole script when a redirection on ':' fails.
	interactive=0
	if (: </dev/tty) 2>/dev/null; then
		interactive=1
	fi

	if [ "$interactive" -eq 0 ]; then
		absent=""
		[ -n "$api_url" ] || absent="$absent --url"
		if [ "$need_token" -eq 1 ] && [ "$token_stdin" -eq 0 ]; then absent="$absent --token-stdin"; fi
		[ "$assume_yes" -eq 1 ] || absent="$absent --yes"
		if [ -n "$absent" ]; then
			die "no terminal to ask on, so pass every input as an option (missing:$absent); nothing was changed"
		fi
	fi

	row() { printf '  %-14s %s\n' "$1" "$2"; }

	# ask QUESTION DEFAULT: reads one line from the terminal into $answer.
	ask() {
		if [ -n "$2" ]; then
			printf '%s [%s]: ' "$1" "$2" >/dev/tty
		else
			printf '%s: ' "$1" >/dev/tty
		fi
		answer=""
		if ! IFS= read -r answer </dev/tty; then
			printf '\n' >/dev/tty
			die "no answer; stopped, and nothing was changed"
		fi
		[ -n "$answer" ] || answer=$2
	}

	if [ -z "$api_url" ]; then
		while :; do
			ask "API address (from the environment's Agents page, ending in /api/v1)" ""
			problem=$(url_problem "$answer")
			[ -n "$problem" ] || break
			warn "$problem"
		done
		api_url=$answer
	fi
	api_url=${api_url%/}

	# Already enrolled and asked nothing more: offer to add this server to
	# another project, as a second agent beside the first.
	if [ "$instance_given" -eq 0 ] && [ "$need_token" -eq 0 ] && [ "$interactive" -eq 1 ] && [ "$assume_yes" -eq 0 ]; then
		say "this server already reports to AutoHeal:"
		for name in $existing_instances; do
			row "$name" "$(describe_instance "$name")"
		done
		ask "Enroll it in another project or environment too? (y/n)" "n"
		case $answer in
		y | Y | yes | Yes | YES)
			while :; do
				ask "Short name for the new agent on this server (for example billing-prod)" ""
				problem=$(instance_problem "$answer")
				if [ -z "$problem" ] && [ "$answer" = default ]; then problem="'default' is the first agent; choose another name"; fi
				if [ -z "$problem" ] && [ -f "$config_dir/$answer.yaml" ]; then
					problem="there is already an agent called $answer here; run with --instance $answer to upgrade it"
				fi
				[ -n "$problem" ] || break
				warn "$problem"
			done
			set_instance "$answer"
			;;
		esac
	fi
	if [ "$have_config" -eq 1 ] && [ -n "$agent_name" ]; then
		warn "keeping the existing config $config_file; --name applies only to a new config (edit its name: line instead)"
	fi

	default_name=$(uname -n)
	if [ "$have_config" -eq 0 ] && [ -z "$agent_name" ]; then
		if [ "$interactive" -eq 1 ] && [ "$assume_yes" -eq 0 ]; then
			while :; do
				ask "Agent name on the dashboard" "$default_name"
				problem=$(name_problem "$answer")
				[ -n "$problem" ] || break
				warn "$problem"
			done
			agent_name=$answer
		else
			agent_name=$default_name
		fi
	fi

	default_user=${previous_user:-$DEFAULT_SERVICE_USER}
	if [ -n "$(user_problem "$default_user")" ]; then default_user=$DEFAULT_SERVICE_USER; fi
	other_instances=""
	for name in $existing_instances; do
		[ "$name" = "$instance" ] || other_instances="$other_instances $name"
	done
	if [ -n "$other_instances" ] && [ -n "$previous_user" ]; then
		# Their folders belong to that user, so every agent here keeps it.
		if [ -n "$service_user" ] && [ "$service_user" != "$previous_user" ]; then
			die "--user: the other agents on this server run as $previous_user, and all of them share one service user"
		fi
		service_user=$previous_user
	fi
	if [ -z "$service_user" ]; then
		if [ "$interactive" -eq 1 ] && [ "$assume_yes" -eq 0 ]; then
			while :; do
				ask "System user for the service" "$default_user"
				problem=$(user_problem "$answer")
				[ -n "$problem" ] || break
				warn "$problem"
			done
			service_user=$answer
		else
			service_user=$default_user
		fi
	fi

	read_token() {
		if [ "$token_stdin" -eq 1 ]; then
			IFS= read -r token || true
			return 0
		fi
		printf 'Enrollment token from the dashboard (paste it; it is not shown): ' >/dev/tty
		if stty -echo </dev/tty 2>/dev/null; then
			restore_tty() { stty echo </dev/tty 2>/dev/null || true; }
		fi
		IFS= read -r token </dev/tty || true
		restore_tty
		restore_tty() { :; }
		printf '\n' >/dev/tty
	}

	# get_token reads a token until it is not empty: three tries on the terminal,
	# one from standard input.
	get_token() {
		token=""
		tries=0
		while [ -z "$token" ]; do
			tries=$((tries + 1))
			read_token
			token=$(printf '%s' "$token" | tr -d ' \t\r\n')
			if [ -z "$token" ]; then
				if [ "$token_stdin" -eq 1 ] || [ "$tries" -ge 3 ]; then
					die "no enrollment token was given; nothing was changed"
				fi
				warn "no token entered; paste the token from the environment's Agents page"
			fi
		done
	}
	if [ "$need_token" -eq 1 ]; then
		get_token
	fi

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

	# --- Confirm -------------------------------------------------------------------

	say "ready to install:"
	row system "$system_name ($arch)"
	if [ -n "$installed_version" ]; then
		row agent "$version (upgrading $installed_version)"
	else
		row agent "$version (new install)"
	fi
	row "API address" "$api_url"
	if [ "$instance" != default ]; then
		row instance "$instance (another agent on this server, beside:$other_instances)"
	fi
	if [ "$have_config" -eq 1 ]; then
		row config "$config_file (kept)"
	else
		row name "$agent_name"
		row config "$config_file (new, host checks only)"
	fi
	if [ -n "$previous_user" ] && [ "$previous_user" != "$service_user" ]; then
		row "service user" "$service_user (was $previous_user, which is left in place)"
	else
		row "service user" "$service_user"
	fi
	if [ "$need_token" -eq 1 ]; then
		row enrollment "with the token given"
	else
		row enrollment "already enrolled; keeping $credential_file"
	fi
	if [ "$start_service" -eq 1 ]; then
		if [ "$os" = linux ]; then row service "systemd unit $unit_name, enabled and started"; else row service "launchd daemon $label"; fi
	else
		row service "not started (--no-start)"
	fi

	if [ "$assume_yes" -eq 0 ]; then
		printf 'Install? [Y/n]: ' >/dev/tty
		answer=""
		if ! IFS= read -r answer </dev/tty; then answer=n; fi
		case $answer in
		"" | y | Y | yes | Yes | YES) ;;
		*) say "stopped; nothing was changed"; exit 1 ;;
		esac
	fi

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
		if ! id "$service_user" >/dev/null 2>&1; then
			if ! getent group "$service_user" >/dev/null 2>&1; then
				groupadd --system "$service_user"
			fi
			say "creating the system user $service_user"
			useradd --system --gid "$service_user" --no-create-home --home-dir "$state_dir" --shell "$nologin" "$service_user"
		fi
	else
		if ! dscl . -read "/Users/$service_user" UniqueID >/dev/null 2>&1; then
			group_id=$(dscl . -read "/Groups/$service_user" PrimaryGroupID 2>/dev/null | awk '{print $2}' || true)
			if [ -z "$group_id" ]; then
				group_id=$(free_darwin_id) || die "no free ID between 400 and 499 for the $service_user group"
				dscl . -create "/Groups/$service_user" PrimaryGroupID "$group_id"
			fi
			say "creating the system user $service_user"
			if dscl . -list /Users UniqueID | awk '{print $2}' | grep -qx "$group_id"; then
				user_id=$(free_darwin_id) || die "no free ID between 400 and 499 for the $service_user user"
			else
				user_id=$group_id
			fi
			dscl . -create "/Users/$service_user" UniqueID "$user_id"
			dscl . -create "/Users/$service_user" PrimaryGroupID "$group_id"
			dscl . -create "/Users/$service_user" UserShell /usr/bin/false
			dscl . -create "/Users/$service_user" NFSHomeDirectory /var/empty
			dscl . -create "/Users/$service_user" IsHidden 1
		fi
	fi
	service_group=$(id -gn "$service_user")
	case $service_group in
	"" | *[!A-Za-z0-9_.-]*) die "the primary group of $service_user ('$service_group') is not a plain group name" ;;
	esac

	# --- Binary, folders and config ----------------------------------------------

	say "installing autoheal-agent $version to $BIN"
	install -d -m 0755 /usr/local/bin
	install -m 0755 "$src/autoheal-agent" "$BIN.new"
	mv -f "$BIN.new" "$BIN"

	install -d -m 0755 "$config_dir"
	install -d -m 0700 -o "$service_user" -g "$service_group" "$state_dir"
	# A folder or credential loosened by hand, or owned by an earlier service
	# user, would make the agent refuse to start.
	chown "$service_user:$service_group" "$state_dir"
	chmod 0700 "$state_dir"
	if [ -f "$credential_file" ]; then
		chown "$service_user:$service_group" "$credential_file"
		chmod 0600 "$credential_file"
	fi

	if [ "$os" = darwin ]; then
		install -d -m 0755 /usr/local/var/log
		[ -f "$log_file" ] || : >"$log_file"
		chown "$service_user:$service_group" "$log_file"
		chmod 0600 "$log_file"
	fi

	install -m 0644 "$src/packaging/agent.example.yaml" "$config_dir/agent.example.yaml"
	if [ "$have_config" -eq 1 ]; then
		say "keeping the existing config $config_file"
	else
		say "writing a starter config to $config_file (host checks only)"
		{
			cat <<'EOF'
# AutoHeal agent configuration, written by install.sh.
#
# It reports CPU, memory and root disk use. Add HTTP and TCP checks for your
# services under `checks:`; agent.example.yaml next to this file describes
# every setting. After editing, run `autoheal-agent validate` and restart the
# service.

EOF
			printf 'name: "%s"\nstate_dir: %s\n' "$agent_name" "$state_dir"
			cat <<'EOF'
log_level: info

host:
  interval: 60s
  cpu: { degraded: 85, down: 95 }
  memory: { degraded: 85, down: 95 }
  disks:
    - { name: root, path: /, degraded: 85, down: 95 }

checks: []
EOF
		} >"$config_file.new"
		chmod 0644 "$config_file.new"
		if ! "$BIN" validate --config "$config_file.new" >/dev/null; then
			rm -f "$config_file.new"
			die "the starter config is not valid (is the name longer than 80 characters?); run again with another --name"
		fi
		mv -f "$config_file.new" "$config_file"
	fi
	"$BIN" validate --config "$config_file" || die "the config $config_file is not valid; fix it and run the script again"

	# --- Enroll ------------------------------------------------------------------

	as_service_user() {
		if [ "$os" = linux ] && have runuser; then
			runuser -u "$service_user" -- "$@"
		elif have sudo; then
			sudo -u "$service_user" -- "$@"
		else
			die "runuser or sudo is needed to enroll as $service_user"
		fi
	}

	if [ "$need_token" -eq 0 ]; then
		say "already enrolled ($credential_file exists); keeping that credential"
		if ! tr -d ' \n' <"$credential_file" | grep -qF "\"api_url\":\"$api_url\""; then
			warn "the saved credential may be for another API address; run again with --reenroll and a new token to switch"
		fi
	else
		attempts=1
		while :; do
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
			if (cd / && printf '%s\n' "$token" | as_service_user "$@" >/dev/null); then
				token=""
				say "enrolled"
				break
			fi
			token=""
			if [ "$token_stdin" -eq 1 ] || [ "$attempts" -ge 3 ]; then
				die "enrollment failed; create a new token on the dashboard and run the script again"
			fi
			attempts=$((attempts + 1))
			warn "try again with a token from the dashboard (attempt $attempts of 3)"
			get_token
		done
	fi

	# --- Service -----------------------------------------------------------------

	if [ "$start_service" -eq 1 ]; then
		if [ "$os" = linux ]; then
			description="AutoHeal agent"
			if [ "$instance" != default ]; then description="AutoHeal agent ($instance)"; fi
			sed -e "s|^Description=.*|Description=$description|" \
				-e "s|^User=.*|User=$service_user|" -e "s|^Group=.*|Group=$service_group|" \
				-e "s|^ExecStart=.*|ExecStart=$BIN run --config $config_file|" \
				-e "s|^StateDirectory=.*|StateDirectory=${state_dir#/var/lib/}|" \
				-e "s|^ReadWritePaths=.*|ReadWritePaths=$state_dir|" \
				"$src/packaging/autoheal-agent.service" >"$work/unit"
			install -m 0644 "$work/unit" "$unit_file"
			systemctl daemon-reload
			systemctl enable "$unit_name" >/dev/null 2>&1
			systemctl restart "$unit_name"
			sleep 2
			systemctl is-active --quiet "$unit_name" ||
				die "the service did not start; see: journalctl -u $unit_name -n 50"
			# The other agents run the binary just replaced.
			for name in $other_instances; do
				systemctl try-restart "$(unit_for "$name")" || warn "could not restart the agent $name"
			done
		else
			cp "$src/packaging/com.autoheal.agent.plist" "$work/agent.plist"
			/usr/libexec/PlistBuddy -c "Set :Label $label" -c "Set :ProgramArguments:3 $config_file" \
				-c "Set :UserName $service_user" -c "Set :GroupName $service_group" \
				-c "Set :StandardOutPath $log_file" -c "Set :StandardErrorPath $log_file" "$work/agent.plist" >/dev/null
			install -m 0644 -o root -g wheel "$work/agent.plist" "$plist"
			launchctl bootout "system/$label" >/dev/null 2>&1 || true
			launchctl bootstrap system "$plist" || die "launchctl could not load $plist"
			sleep 2
			launchctl print "system/$label" 2>/dev/null | grep -q 'state = running' ||
				die "the service did not start; see: tail -n 50 $log_file"
			for name in $other_instances; do
				launchctl kickstart -k "system/$(label_for "$name")" >/dev/null || warn "could not restart the agent $name"
			done
		fi
	fi

	# --- Summary -----------------------------------------------------------------

	printf '\n'
	say "autoheal-agent $version is installed"
	row agent "$(describe_instance "$instance")"
	if [ "$instance" != default ]; then row instance "$instance"; fi
	row "service user" "$service_user"
	row config "$config_file"
	row credential "$credential_file"
	if [ "$start_service" -eq 0 ]; then
		row service "not started (--no-start)"
	elif [ "$os" = linux ]; then
		row logs "journalctl -u $unit_name -f"
	else
		row logs "tail -f $log_file"
	fi
	if [ -n "$other_instances" ]; then
		say "other agents on this server:"
		for name in $other_instances; do
			row "$name" "$(describe_instance "$name")"
		done
	fi
	if [ "$start_service" -eq 0 ]; then
		return 0
	fi
	printf '\n'
	if [ "$os" = linux ]; then
		systemctl --no-pager --lines=10 status "$unit_name" || true
	else
		launchctl print "system/$label" 2>/dev/null | grep -E '^\s*(state|pid|last exit code) =' || true
	fi
	printf '\n'
	say "within a minute it shows as online on the environment's Agents page"
	say "add HTTP and TCP checks to $config_file (docs/installation.md, "4. Configure"), then restart the service"
}

main "$@"
