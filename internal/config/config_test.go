package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, content string) (*Config, error) {
	t.Helper()
	return Parse("agent.yaml", []byte(content))
}

func problems(t *testing.T, err error) string {
	t.Helper()
	var configError *Error
	if !errors.As(err, &configError) {
		t.Fatalf("expected a configuration error, got %v", err)
	}
	return configError.Error()
}

func TestDefaults(t *testing.T) {
	config, err := parse(t, "")
	if err != nil {
		t.Fatalf("empty configuration: %v", err)
	}
	if config.Spool.MaxEvents != 10000 || config.Spool.MaxMB != 20 {
		t.Errorf("spool defaults: %+v", config.Spool)
	}
	if !config.HostEnabled() || config.Host.Interval.Duration != time.Minute {
		t.Errorf("host defaults: %+v", config.Host)
	}
	if config.Host.CPU != (Threshold{85, 95}) || config.Host.Memory != (Threshold{85, 95}) {
		t.Errorf("threshold defaults: %+v %+v", config.Host.CPU, config.Host.Memory)
	}
	if len(config.Host.Disks) != 1 || config.Host.Disks[0].Name != "root" || config.Host.Disks[0].Path != "/" {
		t.Errorf("disk defaults: %+v", config.Host.Disks)
	}
	if !filepath.IsAbs(config.StateDir) || config.LogLevel != "info" {
		t.Errorf("state dir or log level: %q %q", config.StateDir, config.LogLevel)
	}
}

func TestCheckDefaults(t *testing.T) {
	config, err := parse(t, "checks:\n  - name: api\n    http: { url: \"https://127.0.0.1/health\" }\n")
	if err != nil {
		t.Fatal(err)
	}
	check := config.Checks[0]
	if check.Interval.Duration != time.Minute || check.Timeout.Duration != 5*time.Second || check.DownSeverity != "error" {
		t.Errorf("check defaults: %+v", check)
	}
}

func TestExampleConfiguration(t *testing.T) {
	config, err := Load(filepath.Join("..", "..", "packaging", "agent.example.yaml"))
	if err != nil {
		t.Fatalf("example configuration: %v", err)
	}
	if len(config.Checks) == 0 {
		t.Error("the example should show at least one check")
	}
}

func TestUnknownKeyNamesItsLine(t *testing.T) {
	_, err := parse(t, "checks:\n  - name: api\n    intervall: 30s\n    tcp: { address: \"127.0.0.1:80\" }\n")
	message := problems(t, err)
	if !strings.Contains(message, "line 3") || !strings.Contains(message, "intervall") {
		t.Errorf("message should name line 3 and the key: %s", message)
	}
}

func TestInvalidDurationNamesItsLine(t *testing.T) {
	_, err := parse(t, "host:\n  interval: fast\n")
	message := problems(t, err)
	if !strings.Contains(message, "line 2") || !strings.Contains(message, "fast") {
		t.Errorf("message should name line 2 and the value: %s", message)
	}
}

func TestAPIAddressRefused(t *testing.T) {
	_, err := parse(t, "name: web-01\nurl: https://autoheal.example.com\n")
	message := problems(t, err)
	if !strings.Contains(message, "agent.yaml:2: url:") || !strings.Contains(message, "comes from enrollment") {
		t.Errorf("unexpected message: %s", message)
	}
}

func TestValidationProblems(t *testing.T) {
	cases := map[string]struct {
		content string
		want    string
	}{
		"check name with a space": {
			"checks:\n  - name: api health\n    tcp: { address: \"127.0.0.1:80\" }\n",
			"agent.yaml:2: checks[0].name: check names may hold only letters, digits and . _ -, up to 59 characters",
		},
		"check name of 60 characters": {
			"checks:\n  - name: " + strings.Repeat("a", 60) + "\n    tcp: { address: \"127.0.0.1:80\" }\n",
			"up to 59 characters",
		},
		"reserved host name": {
			"checks:\n  - name: host.cpu\n    tcp: { address: \"127.0.0.1:80\" }\n",
			"kept for the host checks",
		},
		"duplicate names": {
			"checks:\n  - name: api\n    tcp: { address: \"127.0.0.1:80\" }\n  - name: api\n    tcp: { address: \"127.0.0.1:81\" }\n",
			"agent.yaml:4: checks[1].name: \"api\" is used by another check",
		},
		"thresholds out of order": {
			"host:\n  cpu: { degraded: 96, down: 90 }\n",
			"agent.yaml:2: host.cpu: degraded (96) must be above 0 and below down (90)",
		},
		"neither http nor tcp": {
			"checks:\n  - name: api\n",
			"checks[0]: set http or tcp",
		},
		"both http and tcp": {
			"checks:\n  - name: api\n    http: { url: \"https://a.test\" }\n    tcp: { address: \"a.test:1\" }\n",
			"set either http or tcp, not both",
		},
		"user information in URL": {
			"checks:\n  - name: api\n    http: { url: \"https://user:secret@api.test/health\" }\n",
			"checks[0].http.url: must not contain a user name or password",
		},
		"ftp URL": {
			"checks:\n  - name: api\n    http: { url: \"ftp://api.test/\" }\n",
			"must use http or https",
		},
		"bad TCP address": {
			"checks:\n  - name: db\n    tcp: { address: \"localhost\" }\n",
			"checks[0].tcp.address: must be host:port",
		},
		"interval too short": {
			"checks:\n  - name: db\n    interval: 5s\n    timeout: 1s\n    tcp: { address: \"127.0.0.1:5432\" }\n",
			"agent.yaml:3: checks[0].interval: must be between 10s and 24h",
		},
		"timeout not below interval": {
			"checks:\n  - name: db\n    interval: 10s\n    timeout: 10s\n    tcp: { address: \"127.0.0.1:5432\" }\n",
			"checks[0].timeout",
		},
		"slow not below timeout": {
			"checks:\n  - name: db\n    timeout: 2s\n    slow: 3s\n    tcp: { address: \"127.0.0.1:5432\" }\n",
			"checks[0].slow: must be shorter than the timeout",
		},
		"unknown severity": {
			"checks:\n  - name: db\n    down_severity: warning\n    tcp: { address: \"127.0.0.1:5432\" }\n",
			"down_severity: must be error or critical",
		},
		"relative state folder": {
			"state_dir: state\n",
			"state_dir: must be an absolute path",
		},
		"invalid host prefix": {
			"host_prefix: Web 01\n",
			"host_prefix:",
		},
		"too many checks": {
			"checks:\n" + manyChecks(51),
			"at most 50 checks are allowed",
		},
		"ca file missing": {
			"ca_file: /nonexistent/ca.pem\n",
			"ca_file: cannot be read",
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parse(t, testCase.content)
			message := problems(t, err)
			if !strings.Contains(message, testCase.want) {
				t.Errorf("want %q in:\n%s", testCase.want, message)
			}
		})
	}
}

func manyChecks(count int) string {
	var builder strings.Builder
	for index := 0; index < count; index++ {
		fmt.Fprintf(&builder, "  - name: check-%d\n    tcp: { address: \"127.0.0.1:1\" }\n", index)
	}
	return builder.String()
}

func TestHostChecksCanBeTurnedOff(t *testing.T) {
	config, err := parse(t, "host:\n  enabled: false\n  cpu: { degraded: 99, down: 1 }\n")
	if err != nil {
		t.Fatalf("disabled host block is not validated: %v", err)
	}
	if config.HostEnabled() {
		t.Error("host checks should be off")
	}
}

func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"web-01":                "web-01",
		"Web 01.Local":          "web-01.local",
		"--Weird__Host--":       "weird__host",
		"":                      "host",
		"***":                   "host",
		strings.Repeat("a", 45): strings.Repeat("a", 40),
	}
	for hostname, want := range cases {
		if got := NormalizePrefix(hostname); got != want {
			t.Errorf("NormalizePrefix(%q) = %q, want %q", hostname, got, want)
		}
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidCAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := parse(t, "ca_file: "+path+"\n")
	if !strings.Contains(problems(t, err), "holds no PEM certificate") {
		t.Errorf("unexpected error: %v", err)
	}
}
