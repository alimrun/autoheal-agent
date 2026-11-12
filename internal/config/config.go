// Package config loads and validates the agent's YAML configuration file.
package config

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	MaxChecks         = 50
	MaxCheckNameRunes = 59
	MaxPrefixRunes    = 40
	MinInterval       = 10 * time.Second
)

var (
	checkNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,59}$`)
	diskNamePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,49}$`)
	prefixPattern    = regexp.MustCompile(`^[a-z0-9._-]{1,40}$`)
	prefixInvalid    = regexp.MustCompile(`[^a-z0-9._-]`)
	dashRuns         = regexp.MustCompile(`-{2,}`)
)

type Config struct {
	Name       string  `yaml:"name"`
	HostPrefix string  `yaml:"host_prefix"`
	StateDir   string  `yaml:"state_dir"`
	CAFile     string  `yaml:"ca_file"`
	LogLevel   string  `yaml:"log_level"`
	Spool      Spool   `yaml:"spool"`
	Host       Host    `yaml:"host"`
	Checks     []Check `yaml:"checks"`

	// Path is the file the configuration was read from.
	Path string `yaml:"-"`
}

type Spool struct {
	MaxEvents int `yaml:"max_events"`
	MaxMB     int `yaml:"max_mb"`
}

type Host struct {
	Enabled  *bool     `yaml:"enabled"`
	Interval Duration  `yaml:"interval"`
	CPU      Threshold `yaml:"cpu"`
	Memory   Threshold `yaml:"memory"`
	Disks    []Disk    `yaml:"disks"`
}

type Threshold struct {
	Degraded float64 `yaml:"degraded"`
	Down     float64 `yaml:"down"`
}

type Disk struct {
	Name     string  `yaml:"name"`
	Path     string  `yaml:"path"`
	Degraded float64 `yaml:"degraded"`
	Down     float64 `yaml:"down"`
}

type Check struct {
	Name         string     `yaml:"name"`
	HTTP         *HTTPCheck `yaml:"http"`
	TCP          *TCPCheck  `yaml:"tcp"`
	Interval     Duration   `yaml:"interval"`
	Timeout      Duration   `yaml:"timeout"`
	Slow         Duration   `yaml:"slow"`
	DownSeverity string     `yaml:"down_severity"`
}

type HTTPCheck struct {
	URL          string `yaml:"url"`
	ExpectStatus []int  `yaml:"expect_status"`
}

type TCPCheck struct {
	Address string `yaml:"address"`
}

// Duration reads Go duration strings such as "30s".
type Duration struct{ time.Duration }

func (duration *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration such as 30s or 5m", node.Line, node.Value)
	}
	duration.Duration = parsed
	return nil
}

// HostEnabled reports whether the host checks run. They do unless turned off.
func (config *Config) HostEnabled() bool {
	return config.Host.Enabled == nil || *config.Host.Enabled
}

func DefaultPath() string {
	if runtime.GOOS == "darwin" {
		return "/usr/local/etc/autoheal/agent.yaml"
	}
	return "/etc/autoheal/agent.yaml"
}

func defaultStateDir() string {
	if runtime.GOOS == "darwin" {
		return "/usr/local/var/autoheal-agent"
	}
	return "/var/lib/autoheal-agent"
}

// Error lists every problem found in a configuration file, each with its line.
type Error struct {
	Path     string
	Problems []Problem
}

type Problem struct {
	Line    int
	Field   string
	Message string
}

func (configError *Error) Error() string {
	lines := make([]string, 0, len(configError.Problems))
	for _, problem := range configError.Problems {
		location := configError.Path
		if problem.Line > 0 {
			location += ":" + strconv.Itoa(problem.Line)
		}
		if problem.Field != "" {
			lines = append(lines, fmt.Sprintf("%s: %s: %s", location, problem.Field, problem.Message))
		} else {
			lines = append(lines, fmt.Sprintf("%s: %s", location, problem.Message))
		}
	}
	return strings.Join(lines, "\n")
}

// Load reads, defaults and validates the configuration file at path.
func Load(path string) (*Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("configuration file %s not found; copy packaging/agent.example.yaml there and edit it", path)
		}
		return nil, fmt.Errorf("reading configuration: %w", err)
	}
	return Parse(path, content)
}

// Parse defaults and validates configuration content read from path.
func Parse(path string, content []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, &Error{Path: path, Problems: []Problem{{Message: yamlMessage(err)}}}
	}

	if problem, found := addressKey(&root); found {
		return nil, &Error{Path: path, Problems: []Problem{problem}}
	}

	config := &Config{}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(config); err != nil && !errors.Is(err, io.EOF) {
		return nil, &Error{Path: path, Problems: []Problem{{Message: yamlMessage(err)}}}
	}

	config.Path = path
	config.applyDefaults()

	validator := &validator{root: &root}
	validator.validate(config)
	if len(validator.problems) > 0 {
		return nil, &Error{Path: path, Problems: validator.problems}
	}

	return config, nil
}

func yamlMessage(err error) string {
	message := strings.TrimPrefix(err.Error(), "yaml: ")
	message = strings.ReplaceAll(message, "unmarshal errors:\n  ", "")
	return strings.ReplaceAll(message, " in type config.", " in ")
}

func addressKey(root *yaml.Node) (Problem, bool) {
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return Problem{}, false
	}
	mapping := root.Content[0]
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		key := mapping.Content[index]
		if key.Value == "url" || key.Value == "api_url" || key.Value == "endpoint" {
			return Problem{Line: key.Line, Field: key.Value, Message: "the API address comes from enrollment and cannot be set here; remove this key"}, true
		}
	}
	return Problem{}, false
}

func (config *Config) applyDefaults() {
	if config.StateDir == "" {
		config.StateDir = defaultStateDir()
	}
	if config.LogLevel == "" {
		config.LogLevel = "info"
	}
	if config.Spool.MaxEvents == 0 {
		config.Spool.MaxEvents = 10000
	}
	if config.Spool.MaxMB == 0 {
		config.Spool.MaxMB = 20
	}
	if config.Host.Interval.Duration == 0 {
		config.Host.Interval.Duration = 60 * time.Second
	}
	defaultThreshold(&config.Host.CPU.Degraded, &config.Host.CPU.Down)
	defaultThreshold(&config.Host.Memory.Degraded, &config.Host.Memory.Down)
	if config.Host.Disks == nil {
		config.Host.Disks = []Disk{{Name: "root", Path: "/"}}
	}
	for index := range config.Host.Disks {
		defaultThreshold(&config.Host.Disks[index].Degraded, &config.Host.Disks[index].Down)
	}
	for index := range config.Checks {
		check := &config.Checks[index]
		if check.Interval.Duration == 0 {
			check.Interval.Duration = 60 * time.Second
		}
		if check.Timeout.Duration == 0 {
			check.Timeout.Duration = 5 * time.Second
		}
		if check.DownSeverity == "" {
			check.DownSeverity = "error"
		}
	}
}

func defaultThreshold(degraded, down *float64) {
	if *degraded == 0 {
		*degraded = 85
	}
	if *down == 0 {
		*down = 95
	}
}

// NormalizePrefix turns a hostname into a check-name prefix.
func NormalizePrefix(hostname string) string {
	prefix := prefixInvalid.ReplaceAllString(strings.ToLower(hostname), "-")
	prefix = dashRuns.ReplaceAllString(prefix, "-")
	prefix = strings.Trim(prefix, "._-")
	if len(prefix) > MaxPrefixRunes {
		prefix = strings.TrimRight(prefix[:MaxPrefixRunes], "._-")
	}
	if prefix == "" {
		return "host"
	}
	return prefix
}

// Prefix is the configured host prefix, or the normalized hostname.
func (config *Config) Prefix(hostname string) string {
	if config.HostPrefix != "" {
		return config.HostPrefix
	}
	return NormalizePrefix(hostname)
}

type validator struct {
	root     *yaml.Node
	problems []Problem
}

func (validator *validator) add(message string, path ...string) {
	validator.problems = append(validator.problems, Problem{
		Line:    lineOf(validator.root, path...),
		Field:   fieldName(path),
		Message: message,
	})
}

func fieldName(path []string) string {
	var builder strings.Builder
	for _, part := range path {
		if _, err := strconv.Atoi(part); err == nil {
			builder.WriteString("[" + part + "]")
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString(".")
		}
		builder.WriteString(part)
	}
	return builder.String()
}

// lineOf finds the line of the deepest node along path that exists.
func lineOf(root *yaml.Node, path ...string) int {
	if root == nil || len(root.Content) == 0 {
		return 0
	}
	node := root.Content[0]
	line := node.Line
	for _, part := range path {
		next := child(node, part)
		if next == nil {
			return line
		}
		node = next
		line = node.Line
	}
	return line
}

func child(node *yaml.Node, part string) *yaml.Node {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == part {
				return node.Content[index+1]
			}
		}
	case yaml.SequenceNode:
		position, err := strconv.Atoi(part)
		if err == nil && position >= 0 && position < len(node.Content) {
			return node.Content[position]
		}
	}
	return nil
}

func (validator *validator) validate(config *Config) {
	if len([]rune(config.Name)) > 80 {
		validator.add("must be at most 80 characters", "name")
	}
	if config.HostPrefix != "" && !prefixPattern.MatchString(config.HostPrefix) {
		validator.add("may hold only lowercase letters, digits and . _ -, up to 40 characters", "host_prefix")
	}
	if !filepath.IsAbs(config.StateDir) {
		validator.add("must be an absolute path", "state_dir")
	}
	if config.CAFile != "" {
		validator.validateCAFile(config.CAFile)
	}
	switch config.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		validator.add("must be debug, info, warn or error", "log_level")
	}
	if config.Spool.MaxEvents < 50 || config.Spool.MaxEvents > 1000000 {
		validator.add("must be between 50 and 1000000", "spool", "max_events")
	}
	if config.Spool.MaxMB < 1 || config.Spool.MaxMB > 10240 {
		validator.add("must be between 1 and 10240", "spool", "max_mb")
	}

	validator.validateHost(config)
	validator.validateChecks(config.Checks)
}

func (validator *validator) validateCAFile(path string) {
	content, err := os.ReadFile(path)
	if err != nil {
		validator.add("cannot be read: "+err.Error(), "ca_file")
		return
	}
	if !x509.NewCertPool().AppendCertsFromPEM(content) {
		validator.add("holds no PEM certificate", "ca_file")
	}
}

func (validator *validator) validateHost(config *Config) {
	if !config.HostEnabled() {
		return
	}
	interval := config.Host.Interval.Duration
	if interval < MinInterval || interval > time.Hour {
		validator.add("must be between 10s and 1h", "host", "interval")
	}
	validator.validateThreshold(config.Host.CPU.Degraded, config.Host.CPU.Down, "host", "cpu")
	validator.validateThreshold(config.Host.Memory.Degraded, config.Host.Memory.Down, "host", "memory")

	seen := map[string]bool{}
	for index, disk := range config.Host.Disks {
		position := strconv.Itoa(index)
		if !diskNamePattern.MatchString(disk.Name) {
			validator.add("may hold only letters, digits and . _ -, up to 49 characters", "host", "disks", position, "name")
		} else if seen[disk.Name] {
			validator.add(fmt.Sprintf("%q is used by another disk", disk.Name), "host", "disks", position, "name")
		}
		seen[disk.Name] = true
		if !filepath.IsAbs(disk.Path) {
			validator.add("must be an absolute path", "host", "disks", position, "path")
		}
		validator.validateThreshold(disk.Degraded, disk.Down, "host", "disks", position)
	}
}

func (validator *validator) validateThreshold(degraded, down float64, path ...string) {
	if degraded <= 0 || down > 100 || degraded >= down {
		validator.add(fmt.Sprintf("degraded (%g) must be above 0 and below down (%g), and down at most 100", degraded, down), path...)
	}
}

func (validator *validator) validateChecks(checks []Check) {
	if len(checks) > MaxChecks {
		validator.add(fmt.Sprintf("at most %d checks are allowed", MaxChecks), "checks")
	}

	seen := map[string]bool{}
	for index, check := range checks {
		position := strconv.Itoa(index)
		switch {
		case !checkNamePattern.MatchString(check.Name):
			validator.add("check names may hold only letters, digits and . _ -, up to 59 characters", "checks", position, "name")
		case strings.HasPrefix(check.Name, "host."):
			validator.add("names starting with \"host.\" are kept for the host checks", "checks", position, "name")
		case seen[check.Name]:
			validator.add(fmt.Sprintf("%q is used by another check", check.Name), "checks", position, "name")
		}
		seen[check.Name] = true

		switch {
		case check.HTTP != nil && check.TCP != nil:
			validator.add("set either http or tcp, not both", "checks", position)
		case check.HTTP != nil:
			validator.validateHTTP(check.HTTP, position)
		case check.TCP != nil:
			validator.validateTCP(check.TCP, position)
		default:
			validator.add("set http or tcp", "checks", position)
		}

		interval := check.Interval.Duration
		if interval < MinInterval || interval > 24*time.Hour {
			validator.add("must be between 10s and 24h", "checks", position, "interval")
		}
		timeout := check.Timeout.Duration
		if timeout < time.Second || timeout > time.Minute || timeout >= interval {
			validator.add("must be between 1s and 60s and shorter than the interval", "checks", position, "timeout")
		}
		if check.Slow.Duration < 0 || (check.Slow.Duration > 0 && check.Slow.Duration >= timeout) {
			validator.add("must be shorter than the timeout", "checks", position, "slow")
		}
		if check.DownSeverity != "error" && check.DownSeverity != "critical" {
			validator.add("must be error or critical", "checks", position, "down_severity")
		}
	}
}

func (validator *validator) validateHTTP(check *HTTPCheck, position string) {
	parsed, err := url.Parse(check.URL)
	switch {
	case err != nil || parsed.Host == "":
		validator.add("must be a full http or https URL", "checks", position, "http", "url")
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		validator.add("must use http or https", "checks", position, "http", "url")
	case parsed.User != nil:
		validator.add("must not contain a user name or password", "checks", position, "http", "url")
	}
	for _, status := range check.ExpectStatus {
		if status < 100 || status > 599 {
			validator.add(fmt.Sprintf("%d is not an HTTP status", status), "checks", position, "http", "expect_status")
		}
	}
}

func (validator *validator) validateTCP(check *TCPCheck, position string) {
	host, port, err := net.SplitHostPort(check.Address)
	number, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || number < 1 || number > 65535 {
		validator.add("must be host:port", "checks", position, "tcp", "address")
	}
}
