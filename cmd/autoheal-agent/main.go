// Command autoheal-agent reports a host's heartbeat and health checks to AutoHeal. It never runs commands.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alimrun/autoheal-agent/internal/api"
	"github.com/alimrun/autoheal-agent/internal/buildinfo"
	"github.com/alimrun/autoheal-agent/internal/check"
	"github.com/alimrun/autoheal-agent/internal/config"
	"github.com/alimrun/autoheal-agent/internal/credential"
	"github.com/alimrun/autoheal-agent/internal/event"
	"github.com/alimrun/autoheal-agent/internal/runner"
	"github.com/alimrun/autoheal-agent/internal/spool"
)

const usage = `Usage:
  autoheal-agent enroll   [--config PATH] [--token-stdin] [--force]
  autoheal-agent run      [--config PATH]
  autoheal-agent validate [--config PATH]
  autoheal-agent version

enroll reads the API address from AUTOHEAL_URL and the enrollment token from
AUTOHEAL_ENROLLMENT_TOKEN, or from standard input with --token-stdin. The token
is never accepted as an argument.
`

type environment struct {
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
}

func main() {
	os.Exit(run(environment{args: os.Args[1:], stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}))
}

func run(env environment) int {
	if len(env.args) == 0 {
		fmt.Fprint(env.stderr, usage)
		return 2
	}

	command, args := env.args[0], env.args[1:]
	switch command {
	case "version", "--version", "-v":
		fmt.Fprintln(env.stdout, buildinfo.Describe())
		return 0
	case "validate":
		return validate(env, args)
	case "enroll":
		return enroll(env, args)
	case "run":
		return runAgent(env, args)
	case "help", "--help", "-h":
		fmt.Fprint(env.stdout, usage)
		return 0
	default:
		fmt.Fprintf(env.stderr, "unknown command %q\n\n%s", command, usage)
		return 2
	}
}

func flags(name string, env environment) (*flag.FlagSet, *string) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(env.stderr)
	path := set.String("config", config.DefaultPath(), "configuration file")
	return set, path
}

func validate(env environment, args []string) int {
	set, path := flags("validate", env)
	if err := set.Parse(args); err != nil || set.NArg() > 0 {
		return 2
	}
	loaded, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(env.stderr, err)
		return 1
	}
	fmt.Fprintf(env.stdout, "%s is valid: %d checks, host checks %s\n", *path, len(loaded.Checks), onOff(loaded.HostEnabled()))
	return 0
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func enroll(env environment, args []string) int {
	set, path := flags("enroll", env)
	tokenStdin := set.Bool("token-stdin", false, "read the enrollment token from standard input")
	force := set.Bool("force", false, "replace an existing credential")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() > 0 {
		fmt.Fprintln(env.stderr, "the enrollment token must not be passed as an argument, where other users can see it; set AUTOHEAL_ENROLLMENT_TOKEN or use --token-stdin")
		return 2
	}

	loaded, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(env.stderr, err)
		return 1
	}

	base, err := api.ValidateBaseURL(env.getenv("AUTOHEAL_URL"))
	if env.getenv("AUTOHEAL_URL") == "" {
		err = errors.New("set AUTOHEAL_URL to the API address shown on the enrollment token page")
	}
	if err != nil {
		fmt.Fprintln(env.stderr, err)
		return 1
	}

	if credential.Exists(loaded.StateDir) && !*force {
		fmt.Fprintf(env.stderr, "this agent is already enrolled (%s exists); run with --force to replace its credential\n", credential.Path(loaded.StateDir))
		return 1
	}

	token, err := enrollmentToken(env, *tokenStdin)
	if err != nil {
		fmt.Fprintln(env.stderr, err)
		return 1
	}
	if err := credential.EnsureDir(loaded.StateDir); err != nil {
		fmt.Fprintln(env.stderr, err)
		return 1
	}

	client, err := api.NewClient(base.String(), api.Options{CAFile: loaded.CAFile, UserAgent: buildinfo.UserAgent()})
	if err != nil {
		fmt.Fprintln(env.stderr, err)
		return 1
	}
	details := agentDetails(loaded)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	registration, err := client.Register(ctx, token, details)
	if err != nil {
		if api.KindOf(err) == api.TokenRefused {
			fmt.Fprintln(env.stderr, "the enrollment token was refused: it may have expired, been cancelled or been used already; create a new one on the environment's page")
		} else {
			fmt.Fprintf(env.stderr, "enrollment failed: %v\n", err)
		}
		return 1
	}

	saved := &credential.Credential{
		APIURL:      base.String(),
		Agent:       credential.Ref(registration.Agent),
		Project:     credential.Ref(registration.Project),
		Environment: credential.Ref(registration.Environment),
		Token:       registration.Credential.Token,
		ExpiresAt:   registration.Credential.ExpiresAt,
	}
	if _, err := credential.Save(loaded.StateDir, saved); err != nil {
		fmt.Fprintf(env.stderr, "enrolled, but the credential could not be saved: %v\nrevoke this agent on the dashboard and enroll again\n", err)
		return 1
	}

	fmt.Fprintf(env.stdout, "Enrolled %q into %s · %s.\nStart the agent with: autoheal-agent run\n",
		registration.Agent.Name, registration.Project.Name, registration.Environment.Name)
	if below, ok := buildinfo.Below(buildinfo.Version, registration.MinimumVersion); ok && below {
		fmt.Fprintf(env.stderr, "warning: this agent (%s) is older than the minimum version AutoHeal supports (%s); upgrade it\n", buildinfo.Version, registration.MinimumVersion)
	}
	return 0
}

func enrollmentToken(env environment, fromStdin bool) (string, error) {
	var token string
	if fromStdin {
		line, err := bufio.NewReader(io.LimitReader(env.stdin, 1024)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading the token from standard input: %w", err)
		}
		token = line
	} else {
		token = env.getenv("AUTOHEAL_ENROLLMENT_TOKEN")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("no enrollment token: set AUTOHEAL_ENROLLMENT_TOKEN or pipe it to --token-stdin")
	}
	if len(token) > 64 {
		return "", errors.New("that is not an enrollment token; copy it again from the token page")
	}
	return token, nil
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown"
	}
	return name
}

func agentDetails(loaded *config.Config) api.Details {
	return api.Details{
		Name:     loaded.Name,
		Hostname: hostname(),
		Version:  buildinfo.Version,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	}
}

func logger(level string, output io.Writer) *slog.Logger {
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: levels[level]}))
}

func runAgent(env environment, args []string) int {
	set, path := flags("run", env)
	if err := set.Parse(args); err != nil || set.NArg() > 0 {
		return 2
	}
	loaded, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(env.stderr, err)
		return 1
	}
	log := logger(loaded.LogLevel, env.stderr)

	saved, stamp, err := credential.Load(loaded.StateDir)
	if err != nil {
		log.Error("cannot start", "error", err.Error())
		return 1
	}
	client, err := api.NewClient(saved.APIURL, api.Options{CAFile: loaded.CAFile, UserAgent: buildinfo.UserAgent()})
	if err != nil {
		log.Error("cannot start", "error", err.Error())
		return 1
	}
	waiting, err := spool.Open(filepath.Join(loaded.StateDir, "spool"), loaded.Spool.MaxEvents, int64(loaded.Spool.MaxMB)<<20, log)
	if err != nil {
		log.Error("cannot start", "error", err.Error())
		return 1
	}
	checks, err := buildChecks(loaded, hostname())
	if err != nil {
		log.Error("cannot start", "error", err.Error())
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	runner.New(runner.Deps{
		Options:    runner.DefaultOptions(),
		Log:        log,
		Client:     client,
		StateDir:   loaded.StateDir,
		Details:    agentDetails(loaded),
		Checks:     checks,
		Spool:      waiting,
		Credential: saved,
		Stamp:      stamp,
	}).Run(ctx)
	return 0
}

func buildChecks(loaded *config.Config, host string) ([]*check.Check, error) {
	prefix := loaded.Prefix(host)
	var checks []*check.Check

	if loaded.HostEnabled() {
		interval := loaded.Host.Interval.Duration
		timeout := min(interval/2, 10*time.Second)
		checks = append(checks,
			&check.Check{
				Name: prefix + ":host.cpu", Kind: check.KindCPU, Interval: interval, Timeout: timeout, DownSeverity: "error",
				Probe: &check.CPUProbe{Degraded: loaded.Host.CPU.Degraded, Down: loaded.Host.CPU.Down, Read: check.ReadCPUTimes},
			},
			&check.Check{
				Name: prefix + ":host.memory", Kind: check.KindMemory, Interval: interval, Timeout: timeout, DownSeverity: "error",
				Probe: &check.PercentProbe{Degraded: loaded.Host.Memory.Degraded, Down: loaded.Host.Memory.Down, Read: check.ReadMemoryPercent, What: "memory"},
			},
		)
		for _, disk := range loaded.Host.Disks {
			checks = append(checks, &check.Check{
				Name: prefix + ":host.disk." + disk.Name, Kind: check.KindDisk, Target: disk.Path, Interval: interval, Timeout: timeout, DownSeverity: "error",
				Probe: &check.PercentProbe{Degraded: disk.Degraded, Down: disk.Down, Read: check.DiskPercentReader(disk.Path), What: "of " + disk.Path},
			})
		}
	}

	roots, err := api.RootCAs(loaded.CAFile)
	if err != nil {
		return nil, err
	}
	httpClient := check.NewHTTPClient(roots, "autoheal-agent/"+buildinfo.Version+" check")

	for _, configured := range loaded.Checks {
		built := &check.Check{
			Name:         prefix + ":" + configured.Name,
			Interval:     configured.Interval.Duration,
			Timeout:      configured.Timeout.Duration,
			DownSeverity: configured.DownSeverity,
		}
		if configured.HTTP != nil {
			built.Kind = check.KindHTTP
			built.Target = event.SanitizeURL(configured.HTTP.URL)
			built.Probe = &check.HTTPProbe{URL: configured.HTTP.URL, ExpectStatus: configured.HTTP.ExpectStatus, Slow: configured.Slow.Duration, Client: httpClient}
		} else {
			built.Kind = check.KindTCP
			built.Target = configured.TCP.Address
			built.Probe = &check.TCPProbe{Address: configured.TCP.Address, Slow: configured.Slow.Duration}
		}
		checks = append(checks, built)
	}
	return checks, nil
}
