package accounts

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Realm names the two Qoder exits.
const (
	RealmCN   = "cn"
	RealmINTL = "intl"
)

// RealmConfig is one region's endpoint table, reverse-engineered from the
// official desktop/CLI client.
type RealmConfig struct {
	Name              string   `json:"name"`
	OpenAPI           string   `json:"openapi"`
	Gateway           string   `json:"gateway"`
	GatewayFallbacks  []string `json:"gateway_fallbacks"`
	SecurityInference string   `json:"security_inference,omitempty"`
	Website           string   `json:"website"`
	Domain            string   `json:"domain"`
	ClientID          string   `json:"client_id"`
	RedirectURI       string   `json:"redirect_uri"`
	UA                string   `json:"ua"`
	HomeDir           string   `json:"home_dir"`
	AppDir            string   `json:"app_dir"`
	SendClientID      bool     `json:"send_client_id"`
	SendRedirectURI   bool     `json:"send_redirect_uri"`
	NonceDashed       bool     `json:"nonce_dashed"`
	HasCheckin        bool     `json:"has_checkin"`
	VPCInstance       string   `json:"vpc_instance,omitempty"`
}

// RealmConfigs is the compiled-in region table.
var RealmConfigs = map[string]RealmConfig{
	RealmCN: {
		Name:            "国内版 (China)",
		OpenAPI:         "https://openapi.qoder.com.cn",
		Gateway:         "https://gateway.qoder.com.cn",
		Website:         "https://qoder.com.cn",
		Domain:          "qoder.com.cn",
		ClientID:        "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
		RedirectURI:     "qoder-work-cn://",
		UA:              "QoderWork/1.1.75",
		HomeDir:         ".qoder-cn",
		AppDir:          "com.qodercn.app.stable",
		SendClientID:    true,
		SendRedirectURI: true,
		NonceDashed:     true,
		HasCheckin:      true,
	},
	RealmINTL: {
		Name:        "国际版 (Global)",
		OpenAPI:     "https://openapi.qoder.sh",
		Gateway:     "https://api3.qoder.sh",
		Website:     "https://qoder.com",
		Domain:      "qoder.com",
		ClientID:    "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		RedirectURI: "qoder://aicoding.aicoding-agent/login-success",
		UA:          "Qoder/1.1.75",
		HomeDir:     ".qoder",
		AppDir:      "com.qoder.app.stable",
		// The intl endpoint split inference across clusters: api3 is the primary
		// inference host, api2 is security inference. api1 has not been in the
		// official candidate list for a long time.
		GatewayFallbacks:  []string{"https://api2.qoder.sh", "https://api1.qoder.sh"},
		SecurityInference: "https://api2.qoder.sh",
		SendClientID:      true,
		SendRedirectURI:   false,
		NonceDashed:       false,
		HasCheckin:        false,
	},
}

// Business endpoints, all mounted on the openapi base with plain Bearer auth
// (no COSY signature).
const (
	PathDevicePoll     = "/api/v1/deviceToken/poll"
	PathDeviceRefresh  = "/api/v1/deviceToken/refresh"
	PathJobExchange    = "/api/v1/jobToken/exchange"
	PathJobRefresh     = "/api/v1/jobToken/refresh"
	PathUserinfo       = "/api/v1/userinfo"
	PathQuota          = "/api/v2/quota/usage"
	PathPlan           = "/api/v2/user/plan"
	PathCheckinStatus  = "/sash/api/v1/me/daily-check-in/status"
	PathCheckinClaim   = "/sash/api/v1/me/daily-check-in/claim"
	PathProEligibility = "/sash/api/v1/me/pro-upgrade/eligibility"
	PathProClaim       = "/sash/api/v1/me/pro-upgrade/claim"
	PathCampaigns      = "/sash/api/v1/me/campaigns"
	PathCampaignReward = "/sash/api/v1/me/campaigns/%s/reward"
	PathCampaignClaim  = "/sash/api/v1/me/campaigns/%s/claim"
	PathCreditSummary  = "/sash/api/v1/ai-conversations/credits-summary"
	PathCreditHeatmap  = "/sash/api/v1/ai-conversations/credits-heatmap"
	PathAchievements   = "/sash/api/v1/me/achievements"
)

// CreditPaths is the candidate matrix used to discover the quota endpoint.
// INTL frequently 404/403s on /api/v2/quota/usage while the website-side
// /sash/api/v1/me/* endpoints do carry data.
var CreditPaths = []string{
	"/api/v2/quota/usage",
	"/api/v1/quota/usage",
	"/api/v2/quota",
	"/api/v2/user/quota",
	"/api/v2/me/quota",
	"/api/v2/credits/usage",
	"/api/v2/credits",
	"/api/v2/billing/quota",
	"/sash/api/v1/me/quota",
	"/sash/api/v1/me/credits",
	"/sash/api/v1/me/usage",
}

// CreditSourceFile records which (host, path) pair worked per realm.
const CreditSourceFile = "credits-source.json"

// VPCDomainSuffix is the enterprise VPC domain tail.
const VPCDomainSuffix = "vpc.qoder.com.cn"

// VPCConfigError holds a startup-time QD_CN_VPC_ENDPOINT parse failure.
var VPCConfigError string

var vpcCN *vpcEndpoints

type vpcEndpoints struct {
	instance string
	openapi  string
	gateway  string
	website  string
	domain   string
}

func init() {
	raw := strings.TrimSpace(os.Getenv("QD_CN_VPC_ENDPOINT"))
	if raw == "" {
		return
	}
	parsed, err := ParseVPCEndpoint(raw)
	if err != nil {
		VPCConfigError = err.Error()
		return
	}
	vpcCN = parsed
	if vpcCN == nil {
		return
	}
	cfg := RealmConfigs[RealmCN]
	cfg.OpenAPI = vpcCN.openapi
	cfg.Gateway = vpcCN.gateway
	cfg.Website = vpcCN.website
	cfg.Domain = vpcCN.domain
	cfg.VPCInstance = vpcCN.instance
	cfg.GatewayFallbacks = nil
	RealmConfigs[RealmCN] = cfg
}

// ParseVPCEndpoint accepts an instance name or one of the three per-service
// VPC domains and derives all four endpoints.
//
//	acme
//	acme.vpc.qoder.com.cn
//	acme-openapi.vpc.qoder.com.cn
//	acme-gateway.vpc.qoder.com.cn
func ParseVPCEndpoint(value string) (*vpcEndpoints, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil, nil
	}
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, "/?#@") {
		return nil, fmt.Errorf("expected an instance name or a %s domain, got %q", VPCDomainSuffix, value)
	}
	instance := strings.ToLower(raw)
	suffix := "." + VPCDomainSuffix
	if strings.HasSuffix(instance, suffix) {
		instance = strings.TrimSuffix(instance, suffix)
		switch {
		case strings.HasSuffix(instance, "-openapi"):
			instance = strings.TrimSuffix(instance, "-openapi")
		case strings.HasSuffix(instance, "-gateway"):
			instance = strings.TrimSuffix(instance, "-gateway")
		}
	}
	if instance == "" || strings.ContainsAny(instance, " :_") {
		return nil, fmt.Errorf("invalid VPC instance %q", value)
	}
	return &vpcEndpoints{
		instance: instance,
		openapi:  "https://" + instance + "-openapi" + suffix,
		gateway:  "https://" + instance + "-gateway" + suffix,
		website:  "https://" + instance + "." + VPCDomainSuffix,
		domain:   instance + "." + VPCDomainSuffix,
	}, nil
}

// NormalizeVPCOverride maps a stored value onto an instance name.
// nil inherits the deployment default; "" explicitly means "public CN".
func NormalizeVPCOverride(value any) (*string, error) {
	if value == nil {
		return nil, nil
	}
	s, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("VPC endpoint must be a string or null")
	}
	parsed, err := ParseVPCEndpoint(s)
	if err != nil {
		return nil, err
	}
	instance := ""
	if parsed != nil {
		instance = parsed.instance
	}
	return &instance, nil
}

// RealmConfigFor returns the effective config for (realm, vpc override).
func RealmConfigFor(realm string, vpc *string) (RealmConfig, error) {
	if realm != RealmCN {
		if vpc != nil && *vpc != "" {
			return RealmConfig{}, fmt.Errorf("VPC is only available for CN accounts")
		}
		cfg := RealmConfigs[RealmINTL]
		return cfg, nil
	}
	cfg := RealmConfigs[RealmCN]
	if vpc == nil {
		if VPCConfigError != "" {
			return RealmConfig{}, fmt.Errorf("%s", VPCConfigError)
		}
		return cfg, nil
	}
	if *vpc == "" {
		// Explicit public CN: reset any deployment-level VPC default.
		base := RealmConfigs[RealmCN]
		base.OpenAPI = "https://openapi.qoder.com.cn"
		base.Gateway = "https://gateway.qoder.com.cn"
		base.Website = "https://qoder.com.cn"
		base.Domain = "qoder.com.cn"
		base.VPCInstance = ""
		base.GatewayFallbacks = nil
		return base, nil
	}
	parsed, err := ParseVPCEndpoint(*vpc)
	if err != nil {
		return RealmConfig{}, err
	}
	if parsed == nil {
		return cfg, nil
	}
	cfg.OpenAPI = parsed.openapi
	cfg.Gateway = parsed.gateway
	cfg.Website = parsed.website
	cfg.Domain = parsed.domain
	cfg.VPCInstance = parsed.instance
	cfg.GatewayFallbacks = nil
	return cfg, nil
}

// GetRealmConfig returns the compiled-in config for a realm.
func GetRealmConfig(realm string) RealmConfig {
	if cfg, ok := RealmConfigs[realm]; ok {
		return cfg
	}
	return RealmConfigs[RealmCN]
}

// GatewayCandidates returns the inference host list for a realm.
//
// The COSY signature only covers the path, so switching hosts after a transport
// failure does not invalidate it. VPC instances have exactly one host: the
// public api1/api2/api3 cluster must never be a fallback for enterprise
// credentials.
func GatewayCandidates(realm string, cfg RealmConfig) []string {
	out := []string{cfg.Gateway}
	if cfg.VPCInstance != "" {
		return out
	}
	seen := map[string]bool{cfg.Gateway: true}
	for _, h := range cfg.GatewayFallbacks {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// OpenAPICandidates returns the business-endpoint base candidates for a realm.
//
// Callers only fall through on connection failure or 404 — a business error is
// not retried against another host.
func OpenAPICandidates(realm string, cfg RealmConfig) []string {
	r := RealmCN
	if realm == RealmINTL {
		r = RealmINTL
	}
	if cfg.VPCInstance != "" {
		return []string{strings.TrimRight(cfg.OpenAPI, "/")}
	}
	var out []string
	add := func(s string) {
		s = strings.TrimRight(strings.TrimSpace(s), "/")
		if s == "" {
			return
		}
		for _, existing := range out {
			if existing == s {
				return
			}
		}
		out = append(out, s)
	}
	add(os.Getenv("QD_" + strings.ToUpper(r) + "_OPENAPI"))
	add(cfg.OpenAPI)
	if r == RealmINTL {
		// The intl campaign platform has a second usable entry point.
		add("https://center.qoder.sh")
	}
	if cfg.Domain != "" {
		add("https://openapi." + cfg.Domain)
	}
	add(cfg.Website)
	return out
}

// DetectRealmFromDomain infers the realm from a credential domain.
func DetectRealmFromDomain(domain string) string {
	d := strings.ToLower(domain)
	if strings.Contains(d, "qoder.sh") ||
		(strings.HasSuffix(d, "qoder.com") && !strings.Contains(d, "qoder.com.cn")) ||
		strings.Contains(d, "qoder.com/") {
		return RealmINTL
	}
	return RealmCN
}

// CLIAuthDirs returns the official CLI credential directories to scan.
func CLIAuthDirs(realm string, vpc *string) []string {
	cfg, _ := RealmConfigFor(realm, vpc)
	root := homeDir(cfg.HomeDir)
	var out []string
	if cfg.VPCInstance != "" {
		out = append(out, root+"/.vpc/"+cfg.VPCInstance+"/.auth")
	}
	out = append(out, root+"/.auth")
	return out
}

// VPCConfigured reports whether a deployment-level VPC default is active.
func VPCConfigured() bool { return vpcCN != nil }

// VPCInstanceName returns the deployment-level VPC instance ("" when none).
func VPCInstanceName() string {
	if vpcCN == nil {
		return ""
	}
	return vpcCN.instance
}

// IsConfiguredVCPHost reports whether host belongs to the configured VPC.
func IsConfiguredVCPHost(host string) bool {
	if vpcCN == nil || host == "" {
		return false
	}
	u, err := url.Parse("https://" + host)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return strings.HasSuffix(h, "."+VPCDomainSuffix) &&
		strings.HasPrefix(h, vpcCN.instance)
}
