// Package server wires the HTTP surface: inference endpoints, the panel API and
// the upstream call path.
package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the gateway's runtime configuration.
type Config struct {
	Host          string
	Port          int
	LAN           bool
	APIKey        string
	SystemPrompt  string
	AccountsDir   string
	UsageDir      string
	PanelPassword string
	PIDFile       string
	DefaultRealm  string
	Version       string

	// MaxPayloadBytes caps a client request body.
	MaxPayloadBytes int64
}

// DefaultSystemPrompt is the sentinel that means "keep the template prompt".
const DefaultSystemPrompt = "You are a helpful assistant."

// LoadConfig reads flags-plus-environment configuration.
func LoadConfig(args []string) (*Config, error) {
	cfg := &Config{
		Host:         firstNonEmpty(os.Getenv("HOST"), "127.0.0.1"),
		Version:      Version,
		SystemPrompt: DefaultSystemPrompt,
		DefaultRealm: firstNonEmpty(os.Getenv("QD_PROXY_DEFAULT_REALM"), "cn"),
	}
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			// Silently falling back to the default would start the gateway on a
			// port the operator is not connecting to. Say so instead.
			return nil, fmt.Errorf("invalid PORT %q: must be 1-65535", v)
		}
		cfg.Port = n
	}
	if cfg.Port == 0 {
		cfg.Port = 18090
	}
	cfg.AccountsDir = firstNonEmpty(os.Getenv("ACCOUNTS_DIR"), "accounts")
	cfg.UsageDir = firstNonEmpty(os.Getenv("QD_PROXY_USAGE_DIR"), "usage")
	cfg.APIKey = firstNonEmpty(os.Getenv("API_KEY"), os.Getenv("QD_PROXY_KEY"))
	cfg.MaxPayloadBytes = 50 * 1024 * 1024
	if v := strings.TrimSpace(os.Getenv("QD_MAX_PAYLOAD_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MaxPayloadBytes = n
		}
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch {
		case arg == "--host":
			v, err := next()
			if err != nil {
				return nil, err
			}
			cfg.Host = v
		case arg == "--port":
			v, err := next()
			if err != nil {
				return nil, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > 65535 {
				return nil, fmt.Errorf("invalid --port %q", v)
			}
			cfg.Port = n
		case arg == "--lan":
			cfg.LAN = true
		case arg == "--api-key":
			v, err := next()
			if err != nil {
				return nil, err
			}
			cfg.APIKey = v
		case arg == "--system-prompt":
			v, err := next()
			if err != nil {
				return nil, err
			}
			cfg.SystemPrompt = v
		case arg == "--accounts-dir":
			v, err := next()
			if err != nil {
				return nil, err
			}
			cfg.AccountsDir = v
		case arg == "--usage-dir":
			v, err := next()
			if err != nil {
				return nil, err
			}
			cfg.UsageDir = v
		case arg == "--panel-password":
			v, err := next()
			if err != nil {
				return nil, err
			}
			cfg.PanelPassword = v
		case arg == "--pid-file":
			v, err := next()
			if err != nil {
				return nil, err
			}
			cfg.PIDFile = v
		case arg == "--version", arg == "-version":
			return nil, ErrShowVersion
		case arg == "--help", arg == "-h":
			return nil, ErrShowHelp
		default:
			return nil, fmt.Errorf("unknown argument %q", arg)
		}
	}
	if cfg.LAN && cfg.Host == "127.0.0.1" {
		cfg.Host = "0.0.0.0"
	}
	accountsAbs, err := filepath.Abs(cfg.AccountsDir)
	if err == nil {
		cfg.AccountsDir = accountsAbs
	}
	usageAbs, err := filepath.Abs(cfg.UsageDir)
	if err == nil {
		cfg.UsageDir = usageAbs
	}
	return cfg, nil
}

// Usage describes the CLI.
const Usage = `qoder-proxy — Qoder (qoder.com.cn / qoder.com) OpenAI-compatible gateway

  qoder-proxy [--host HOST] [--port PORT] [--lan] [--api-key KEY]
              [--accounts-dir DIR] [--usage-dir DIR]
              [--panel-password PWD] [--system-prompt TEXT] [--pid-file FILE]`

// Sentinel errors for --version / --help.
var (
	ErrShowVersion = fmt.Errorf("show version")
	ErrShowHelp    = fmt.Errorf("show help")
)

// Version is overridden at build time with -ldflags.
var Version = "2.0.0-go"

// StartedAt is process start time (set by main).
var StartedAt = time.Now()

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func envBool(name string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return def
	}
	switch raw {
	case "1", "on", "true", "yes", "enable", "enabled":
		return true
	case "0", "off", "false", "no", "disable", "disabled":
		return false
	}
	return def
}

func envIntDef(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
