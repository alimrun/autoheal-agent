// Package buildinfo holds the version stamped in at build time and compares versions.
package buildinfo

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// Set with -ldflags "-X github.com/alimrun/autoheal-agent/internal/buildinfo.Version=…".
var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func UserAgent() string {
	return fmt.Sprintf("autoheal-agent/%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH)
}

func Describe() string {
	return fmt.Sprintf("autoheal-agent %s (commit %s, built %s, %s/%s)", Version, Commit, Date, runtime.GOOS, runtime.GOARCH)
}

type semver struct {
	core       [3]int
	prerelease string
}

func parse(version string) (semver, bool) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if plus := strings.IndexByte(version, '+'); plus >= 0 {
		version = version[:plus]
	}

	var parsed semver
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		parsed.prerelease = version[dash+1:]
		version = version[:dash]
	}

	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return semver{}, false
		}
		parsed.core[index] = number
	}

	return parsed, true
}

// Below reports whether version is lower than minimum. ok is false when either cannot be parsed.
func Below(version, minimum string) (below bool, ok bool) {
	current, currentOK := parse(version)
	required, requiredOK := parse(minimum)
	if !currentOK || !requiredOK {
		return false, false
	}

	for index := range current.core {
		if current.core[index] != required.core[index] {
			return current.core[index] < required.core[index], true
		}
	}

	switch {
	case current.prerelease == required.prerelease:
		return false, true
	case current.prerelease == "":
		return false, true
	case required.prerelease == "":
		return true, true
	default:
		return current.prerelease < required.prerelease, true
	}
}
