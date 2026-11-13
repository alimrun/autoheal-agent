#!/usr/bin/env bash
# Build, package, checksum and sign an AutoHeal agent release (design decision 16).
#
#   scripts/release.sh <version>          e.g. scripts/release.sh 0.1.0
#
# Needs a clean working tree with the tag v<version> on HEAD. Writes:
#
#   dist/<os>_<arch>/autoheal-agent                       the four binaries
#   dist/autoheal-agent_<version>_<os>_<arch>.tar.gz      the four archives
#   dist/SHA256SUMS                                       their checksums
#   dist/SHA256SUMS.minisig                               the signature
#
# Signing uses the secret key file named by MINISIGN_SECRET_KEY. Without it the
# script stops after SHA256SUMS, signs nothing, and exits with status 3.
#
# The archives are reproducible: the same commit gives byte-identical archives
# on the same platform. Entries are sorted, owned by 0:0, have fixed modes and
# carry the commit time as their mtime; gzip stores no name or time.

set -euo pipefail

die() { echo "release: $*" >&2; exit 1; }

[ $# -eq 1 ] || die "usage: scripts/release.sh <version>   (for example 0.1.0)"
version=$1
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
	die "version must look like 1.2.3 or 1.2.3-rc.1, without a leading v (got '$version')"

cd "$(dirname "$0")/.."
root=$(pwd -P)

command -v go >/dev/null || die "go is not installed"
command -v shasum >/dev/null || die "shasum is not installed"

# A clean tree, and the tag on HEAD.
[ -z "$(git status --porcelain)" ] || die "the working tree is not clean; commit or stash first (git status)"
head=$(git rev-parse HEAD)
tagged=$(git rev-parse -q --verify "refs/tags/v$version^{commit}" 2>/dev/null) ||
	die "tag v$version does not exist; create it with: git tag -a v$version -m 'autoheal-agent $version'"
[ "$tagged" = "$head" ] || die "tag v$version points at ${tagged:0:12}, not at HEAD ${head:0:12}"

commit=$(git rev-parse --short=12 HEAD)
commit_date=$(git log -1 --format=%cI HEAD)
commit_epoch=$(git log -1 --format=%ct HEAD)

# touch -t wants [[CC]YY]MMDDhhmm[.SS]; GNU and BSD date spell "from epoch" differently.
if stamp=$(date -u -d "@$commit_epoch" +%Y%m%d%H%M.%S 2>/dev/null); then :; else
	stamp=$(date -u -r "$commit_epoch" +%Y%m%d%H%M.%S)
fi

# GNU tar and bsdtar (macOS) need different flags for the same result.
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
	tar_flags=(--format=ustar --no-recursion --owner=0 --group=0 --numeric-owner)
else
	tar_flags=(--format ustar --no-recursion --uid 0 --gid 0 --uname root --gname root --no-xattrs --no-acls --no-fflags)
fi
export COPYFILE_DISABLE=1 # no AppleDouble ._ files from macOS

pkg=github.com/alimrun/autoheal-agent/internal/buildinfo
ldflags="-s -w -X $pkg.Version=$version -X $pkg.Commit=$commit -X $pkg.Date=$commit_date"
targets=(linux_amd64 linux_arm64 darwin_amd64 darwin_arm64)

rm -rf -- "${root:?}/dist"
mkdir -p dist

# Files every archive holds besides the binary: the README with its docs and
# changelog, and LICENSE and minisign.pub once the owner adds them.
[ -f README.md ] && [ -d packaging ] || die "README.md and packaging/ must exist"
extras=(README.md packaging)
[ -f LICENSE ] && extras+=(LICENSE)
[ -f CHANGELOG.md ] && extras+=(CHANGELOG.md)
[ -d docs ] && extras+=(docs)
[ -f minisign.pub ] && extras+=(minisign.pub)

for target in "${targets[@]}"; do
	goos=${target%_*}
	goarch=${target#*_}
	echo "release: building $goos/$goarch"
	env -u GOFLAGS CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOAMD64=v1 GOARM64=v8.0 \
		go build -trimpath -mod=readonly -ldflags "$ldflags" \
		-o "dist/$target/autoheal-agent" ./cmd/autoheal-agent

	name="autoheal-agent_${version}_${target}"
	stage="dist/.stage/$name"
	mkdir -p "$stage"
	cp "dist/$target/autoheal-agent" "$stage/"
	cp -R "${extras[@]}" "$stage/"

	# Fixed modes and times, so the umask and checkout time do not leak in.
	find "$stage" -type d -exec chmod 0755 {} +
	find "$stage" -type f -exec chmod 0644 {} +
	chmod 0755 "$stage/autoheal-agent"
	find "$stage" -exec env TZ=UTC touch -h -t "$stamp" {} +

	(
		cd dist/.stage
		find "$name" -print | LC_ALL=C sort >"../$name.list"
		tar "${tar_flags[@]}" -cf - -T "../$name.list" | gzip -n -9 >"../$name.tar.gz"
	)
	rm -f "dist/$name.list"
done
rm -rf -- "${root:?}/dist/.stage"

(
	cd dist
	LC_ALL=C shasum -a 256 autoheal-agent_"${version}"_*.tar.gz >SHA256SUMS
)
echo "release: wrote dist/SHA256SUMS"
cat dist/SHA256SUMS

if [ -z "${MINISIGN_SECRET_KEY:-}" ]; then
	cat >&2 <<EOF
release: MINISIGN_SECRET_KEY is not set, so nothing was signed.
release: dist/ holds an UNSIGNED preview of $version. To sign, set
release: MINISIGN_SECRET_KEY to the path of the release secret key and run again.
EOF
	exit 3
fi
command -v minisign >/dev/null || die "minisign is not installed (brew install minisign)"
[ -f "$MINISIGN_SECRET_KEY" ] || die "MINISIGN_SECRET_KEY does not name a file"

minisign -S -s "$MINISIGN_SECRET_KEY" -m dist/SHA256SUMS -t "autoheal-agent $version"
echo "release: signed dist/SHA256SUMS -> dist/SHA256SUMS.minisig"

if [ -f minisign.pub ]; then
	minisign -Vm dist/SHA256SUMS -p minisign.pub >/dev/null ||
		die "the signature does not verify with the repository's minisign.pub; wrong secret key?"
	echo "release: signature verified with minisign.pub"
else
	echo "release: no minisign.pub in the repository; users cannot verify this signature yet" >&2
fi
