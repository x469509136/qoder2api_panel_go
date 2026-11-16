package sign

import (
	"os"
	"strings"
	"sync"

	"github.com/xzs/bridge_qoder/internal/fingerprint"
)

// COSY protocol knobs, aligned with the official qodercli 1.1.75. Every one of
// them can be overridden from the environment so operators can copy their own
// CLI identity verbatim without patching code.
const (
	// DefaultCosyVersion is the cosy-version header and payload value.
	DefaultCosyVersion = "1.1.75"
	// DefaultCosyUA is used because the official CLI does not set a User-Agent:
	// the signature WASM header table has no entry, and the bundled undici fetch
	// fills in its default.
	DefaultCosyUA = "undici"
	// DefaultCosyClientType is the client type the upstream authorises models
	// against (CLI = 5, QoderWork = 6, desktop = 10).
	DefaultCosyClientType = "5"
)

var (
	cosyOnce sync.Once
	cosyCfg  struct {
		version    string
		ua         string
		clientType string
		osHeaders  bool
		machineOS  string
	}
)

func loadCosy() {
	cosyOnce.Do(func() {
		cosyCfg.version = envOr("QD_COSY_VERSION", DefaultCosyVersion)
		cosyCfg.ua = envOr("QD_COSY_UA", DefaultCosyUA)
		cosyCfg.clientType = envOr("QD_COSY_CLIENTTYPE", DefaultCosyClientType)
		raw := strings.ToLower(strings.TrimSpace(os.Getenv("QD_COSY_OS_HEADERS")))
		switch raw {
		case "0", "off", "false", "no", "disable", "disabled":
			cosyCfg.osHeaders = false
		default:
			cosyCfg.osHeaders = true
		}
		cosyCfg.machineOS = fingerprint.MachineOS()
	})
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// CosyVersion returns the effective cosy-version.
func CosyVersion() string { loadCosy(); return cosyCfg.version }

// CosyUserAgent returns the effective User-Agent for inference requests.
func CosyUserAgent() string { loadCosy(); return cosyCfg.ua }

// CosyClientType returns the effective cosy-clienttype.
func CosyClientType() string { loadCosy(); return cosyCfg.clientType }

// CosyOSHeadersEnabled reports whether cosy-machineos (and, for SSE,
// cosy-machinehostname) are sent.
func CosyOSHeadersEnabled() bool { loadCosy(); return cosyCfg.osHeaders }

// CosyMachineOS returns the effective cosy-machineos value.
func CosyMachineOS() string { loadCosy(); return cosyCfg.machineOS }
