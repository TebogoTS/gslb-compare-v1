package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Endpoint is one Rancher management server.
type Endpoint struct {
	Name                  string `json:"name"`
	Role                  string `json:"role"` // "legacy" (RKE1) or "target" (RKE2)
	URL                   string `json:"url"`
	TokenEnv              string `json:"tokenEnv"`       // env var holding "token-xxxxx:secret"
	Slot                  string `json:"slot,omitempty"` // target only: "env/dc" of every cluster on this endpoint; beats naming.target
	CAFile                string `json:"caFile,omitempty"`
	InsecureSkipTLSVerify bool   `json:"insecureSkipTLSVerify,omitempty"`
	ClusterInclude        string `json:"clusterInclude,omitempty"` // regex on display name
	ClusterExclude        string `json:"clusterExclude,omitempty"`
	IncludeLocal          bool   `json:"includeLocal,omitempty"`
}

type Naming struct {
	// Named groups: legacy needs base, env, dc. target needs env, dc (pool optional).
	Legacy     string            `json:"legacy"`
	Target     string            `json:"target"`
	EnvAliases map[string]string `json:"envAliases"`
	DCAliases  map[string]string `json:"dcAliases"`
	// Escape hatch: cluster display name -> "env/dc"
	SlotOverrides map[string]string `json:"slotOverrides"`
}

type HostRewrite struct {
	From string `json:"from"` // suffix on the RKE1 host
	To   string `json:"to"`   // replacement suffix used for matching against RKE2
}

type Config struct {
	Endpoints        []Endpoint    `json:"endpoints"`
	Naming           Naming        `json:"naming"`
	LegacyAnnotation string        `json:"legacyAnnotation"`
	Slots            []string      `json:"slots"`
	IgnoreFields     []string      `json:"ignoreFields"`
	HostRewrites     []HostRewrite `json:"hostRewrites"`
	Workers          int           `json:"workers"`
	TimeoutSeconds   int           `json:"timeoutSeconds"`
	AllStates        bool          `json:"allStates"` // also try clusters that are not "active"

	legacyRe, targetRe *regexp.Regexp
	endpointSlot       map[string]string // endpoint name -> normalised Endpoint.Slot
}

var defaultEnvAliases = map[string]string{
	"nonprod": "nonprod", "non-prod": "nonprod", "np": "nonprod", "dev": "nonprod",
	"prod": "prod", "prd": "prod", "pd": "prod",
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Naming.Legacy == "" {
		// base is non-greedy so "x-non-prod-270" yields env "non-prod", not base "x-non" + env "prod".
		c.Naming.Legacy = `(?i)^(?P<base>.+?)-(?P<env>nonprod|non-prod|np|prod|prd|pd)-(?P<dc>270|sdc)$`
	}
	if c.Naming.Target == "" {
		c.Naming.Target = `(?i)^(?P<pool>[a-z]*?)(?P<env>np|pd)(?P<dc>270|sdc|moz)-cap-\d+$`
	}
	// Config aliases are merged over the defaults so a partial map can't silently drop
	// e.g. "non-prod" -> "nonprod". Keys are lower-cased because normEnv/normDC look up lower-case.
	env := map[string]string{}
	for k, v := range defaultEnvAliases {
		env[k] = v
	}
	for k, v := range c.Naming.EnvAliases {
		env[strings.ToLower(k)] = strings.ToLower(v)
	}
	c.Naming.EnvAliases = env
	dc := map[string]string{}
	for k, v := range c.Naming.DCAliases {
		dc[strings.ToLower(k)] = strings.ToLower(v)
	}
	c.Naming.DCAliases = dc
	if c.LegacyAnnotation == "" {
		c.LegacyAnnotation = "clusterpool/legacy-cluster"
	}
	if len(c.Slots) == 0 {
		c.Slots = []string{"nonprod/270", "nonprod/sdc", "prod/270", "prod/sdc"}
	}
	if c.Workers <= 0 {
		c.Workers = 10
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 120
	}
	if c.legacyRe, err = regexp.Compile(c.Naming.Legacy); err != nil {
		return nil, fmt.Errorf("naming.legacy: %w", err)
	}
	if c.targetRe, err = regexp.Compile(c.Naming.Target); err != nil {
		return nil, fmt.Errorf("naming.target: %w", err)
	}
	for _, g := range []string{"base", "env", "dc"} {
		if c.legacyRe.SubexpIndex(g) < 0 {
			return nil, fmt.Errorf("naming.legacy must have named group (?P<%s>...)", g)
		}
	}
	for _, g := range []string{"env", "dc"} {
		if c.targetRe.SubexpIndex(g) < 0 {
			return nil, fmt.Errorf("naming.target must have named group (?P<%s>...)", g)
		}
	}
	seen := map[string]bool{}
	c.endpointSlot = map[string]string{}
	for i, ep := range c.Endpoints {
		if ep.Role != "legacy" && ep.Role != "target" {
			return nil, fmt.Errorf("endpoints[%d] %q: role must be legacy or target", i, ep.Name)
		}
		if ep.Name == "" || seen[ep.Name] {
			return nil, fmt.Errorf("endpoints[%d]: name must be set and unique", i)
		}
		seen[ep.Name] = true
		if ep.URL == "" || ep.TokenEnv == "" {
			return nil, fmt.Errorf("endpoint %q: url and tokenEnv are required", ep.Name)
		}
		if ep.Slot != "" {
			if ep.Role != "target" {
				return nil, fmt.Errorf("endpoint %q: slot is only supported on target endpoints", ep.Name)
			}
			parts := strings.SplitN(strings.ToLower(strings.TrimSpace(ep.Slot)), "/", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("endpoint %q: slot must be \"env/dc\", got %q", ep.Name, ep.Slot)
			}
			c.endpointSlot[ep.Name] = c.normEnv(parts[0]) + "/" + c.normDC(parts[1])
		}
	}
	return &c, nil
}

func (c *Config) normEnv(s string) string {
	s = strings.ToLower(s)
	if v, ok := c.Naming.EnvAliases[s]; ok {
		return v
	}
	return s
}

func (c *Config) normDC(s string) string {
	s = strings.ToLower(s)
	if v, ok := c.Naming.DCAliases[s]; ok {
		return v
	}
	return s
}

// parseLegacy returns base, slot ("env/dc").
func (c *Config) parseLegacy(name string) (base, slot string, ok bool) {
	m := c.legacyRe.FindStringSubmatch(name)
	if m == nil {
		return "", "", false
	}
	base = strings.ToLower(m[c.legacyRe.SubexpIndex("base")])
	slot = c.normEnv(m[c.legacyRe.SubexpIndex("env")]) + "/" + c.normDC(m[c.legacyRe.SubexpIndex("dc")])
	if o, ok := c.Naming.SlotOverrides[name]; ok {
		slot = o
	}
	return base, slot, true
}

// targetSlot returns "env/dc" or "" when unknown. Precedence: naming.slotOverrides
// (per cluster) > endpoint slot (per management server) > naming.target regex.
func (c *Config) targetSlot(endpoint, name string) string {
	if o, ok := c.Naming.SlotOverrides[name]; ok {
		return o
	}
	if s, ok := c.endpointSlot[endpoint]; ok {
		return s
	}
	m := c.targetRe.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return c.normEnv(m[c.targetRe.SubexpIndex("env")]) + "/" + c.normDC(m[c.targetRe.SubexpIndex("dc")])
}

func (c *Config) rewriteHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	for _, r := range c.HostRewrites {
		if r.From != "" && strings.HasSuffix(h, strings.ToLower(r.From)) {
			return strings.TrimSuffix(h, strings.ToLower(r.From)) + strings.ToLower(r.To)
		}
	}
	return h
}

// ---- snapshot model ----

type Snapshot struct {
	CollectedAt string        `json:"collectedAt"`
	Clusters    []ClusterData `json:"clusters"`
}

type ClusterData struct {
	Endpoint   string            `json:"endpoint"`
	Role       string            `json:"role"`
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	State      string            `json:"state"`
	Labels     map[string]string `json:"labels,omitempty"`
	LegacyHint string            `json:"legacyHint,omitempty"`
	HasK8gb    bool              `json:"hasK8gb"`
	Skipped    string            `json:"skipped,omitempty"`
	Error      string            `json:"error,omitempty"`
	Gslbs      []GslbRecord      `json:"gslbs,omitempty"`
}

type GslbRecord struct {
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
	Mode      string       `json:"mode"` // embedded | resourceRef | ingress-annotation
	Strategy  Strategy     `json:"strategy"`
	GeoTag    string       `json:"geoTag,omitempty"`
	Hosts     []HostRecord `json:"hosts"`
}

type Strategy struct {
	Type                       string            `json:"type,omitempty"`
	PrimaryGeoTag              string            `json:"primaryGeoTag,omitempty"`
	DNSTtlSeconds              int64             `json:"dnsTtlSeconds,omitempty"`
	SplitBrainThresholdSeconds int64             `json:"splitBrainThresholdSeconds,omitempty"`
	Weight                     map[string]string `json:"weight,omitempty"`
}

type HostRecord struct {
	Host         string   `json:"host"`
	Ingress      string   `json:"ingress"`
	IngressClass string   `json:"ingressClass,omitempty"`
	TLSSecret    string   `json:"tlsSecret,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	Health       string   `json:"health,omitempty"`
	Records      []string `json:"records,omitempty"`
}
