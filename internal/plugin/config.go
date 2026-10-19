package plugin

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// config is the plugin-owned configuration tree under
// plugins.configs.<pluginID>. The host keeps enabled/priority for itself and
// passes everything else through config_yaml verbatim.
type config struct {
	// Enabled mirrors plugins.configs.<pluginID>.enabled for the plugin's own
	// early exits; the host already gates dispatch, this is belt and braces.
	Enabled bool `yaml:"enabled"`

	// BaseURL is the OpenAI-compatible provider surface
	// (default https://api.commandcode.ai/provider/v1).
	BaseURL string `yaml:"base-url"`

	// GenerateURL overrides the CLI gateway
	// (default {base-url root}/alpha/generate). Go / GOAT / Pro subscriptions
	// are rejected by the Provider API with 403 upgrade_required and are only
	// servable through this gateway.
	GenerateURL string `yaml:"generate-url"`

	// CLIVersion is the x-command-code-version header sent to the CLI gateway.
	// It must track the published command-code package.
	CLIVersion string `yaml:"cli-version"`

	// WorkingDir is the project directory reported to the CLI gateway. Empty
	// uses the core process's working directory.
	WorkingDir string `yaml:"working-dir"`

	// CatalogURL overrides the model catalog address (default {base-url}/models).
	CatalogURL string `yaml:"catalog-url"`

	// APIKeys is the ordered key pool used when no matching auth record is
	// selected (router path passes nil auth). Each entry supports ${ENV_VAR}
	// expansion. When both this and an auth record are present, the selected
	// auth record wins and the pool is the failover order behind it.
	APIKeys []string `yaml:"api-keys"`

	// PlanFilter, when set, hides models whose minimum plan weight exceeds it
	// from the registered model list ("go" keeps only Go-tier models, "" keeps
	// every model the catalog reports).
	PlanFilter string `yaml:"plan-filter"`

	// VisibleModels, when non-empty, keeps only the listed catalog ids in the
	// registered model list (the allowlist twin of the dsh plugin's picker).
	VisibleModels []string `yaml:"visible-models"`

	// ExcludeHidden drops the catalog's hidden rows (default true).
	ExcludeHidden *bool `yaml:"exclude-hidden"`

	// RequestTimeout bounds one upstream HTTP attempt (default 5m).
	RequestTimeout time.Duration `yaml:"request-timeout"`

	// AllowHTTP permits http:// upstream bases (local testing only).
	AllowHTTP bool `yaml:"allow-http"`

	// ModelPrefix controls the client-visible model id prefix. When enabled
	// (default) clients address "commandcode/<upstream id>".
	ModelPrefix struct {
		Enabled *bool  `yaml:"enabled"`
		Value   string `yaml:"value"`
	} `yaml:"model-prefix"`
}

// defaultConfig returns the configuration used when no config_yaml is present.
func defaultConfig() config {
	tru := true
	cfg := config{
		Enabled:        true,
		BaseURL:        DefaultBaseURL,
		CLIVersion:     DefaultCLIVersion,
		RequestTimeout: 5 * time.Minute,
		ExcludeHidden:  &tru,
	}
	cfg.ModelPrefix.Enabled = &tru
	cfg.ModelPrefix.Value = ProviderID
	return cfg
}

// loadConfig decodes config_yaml over the defaults and validates the fields
// this plugin owns. The host keeps enabled/priority, so their presence here is
// informational only.
func loadConfig(raw []byte) (config, error) {
	cfg := defaultConfig()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return config{}, fmt.Errorf("plugins.configs.%s: invalid yaml: %w", PluginName, err)
		}
	}
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if strings.Contains(cfg.BaseURL, "?") || strings.Contains(cfg.BaseURL, "#") {
		return config{}, fmt.Errorf("plugins.configs.%s: base-url must not carry query or fragment", PluginName)
	}
	if !cfg.AllowHTTP && !strings.HasPrefix(cfg.BaseURL, "https://") {
		return config{}, fmt.Errorf("plugins.configs.%s: base-url must be https:// (set allow-http: true for local testing)", PluginName)
	}
	cfg.GenerateURL = strings.TrimSpace(cfg.GenerateURL)
	if cfg.GenerateURL != "" && !cfg.AllowHTTP && !strings.HasPrefix(cfg.GenerateURL, "https://") {
		return config{}, fmt.Errorf("plugins.configs.%s: generate-url must be https:// (set allow-http: true for local testing)", PluginName)
	}
	cfg.CLIVersion = strings.TrimSpace(cfg.CLIVersion)
	if cfg.CLIVersion == "" {
		cfg.CLIVersion = DefaultCLIVersion
	}
	cfg.WorkingDir = strings.TrimSpace(cfg.WorkingDir)
	cfg.CatalogURL = strings.TrimSpace(cfg.CatalogURL)
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Minute
	}
	if cfg.ModelPrefix.Value == "" {
		cfg.ModelPrefix.Value = ProviderID
	}
	cfg.PlanFilter = planFilterAlias(normalizePlanID(cfg.PlanFilter))
	if cfg.PlanFilter != "" {
		if _, ok := planLabel[cfg.PlanFilter]; !ok {
			return config{}, fmt.Errorf("plugins.configs.%s: unknown plan-filter %q (want one of go, goat, pro, provider, max)", PluginName, cfg.PlanFilter)
		}
	}
	if cfg.ExcludeHidden == nil {
		tru := true
		cfg.ExcludeHidden = &tru
	}
	if cfg.ModelPrefix.Enabled == nil {
		tru := true
		cfg.ModelPrefix.Enabled = &tru
	}

	// Expand ${ENV_VAR} in the key pool; an unresolvable variable keeps the
	// placeholder so the executor reports a missing key instead of sending a
	// literal placeholder upstream.
	expanded := make([]string, 0, len(cfg.APIKeys))
	seen := map[string]bool{}
	for _, key := range cfg.APIKeys {
		key = expandEnv(strings.TrimSpace(key))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		expanded = append(expanded, key)
	}
	cfg.APIKeys = expanded

	visible := make([]string, 0, len(cfg.VisibleModels))
	seenModel := map[string]bool{}
	for _, id := range cfg.VisibleModels {
		id = strings.TrimSpace(id)
		if id == "" || seenModel[id] {
			continue
		}
		seenModel[id] = true
		visible = append(visible, id)
	}
	cfg.VisibleModels = visible
	return cfg, nil
}

// planFilterAlias maps the short picker names ("go", "goat", "pro",
// "provider", "max") to the full plan ids the tables are keyed by, so the
// config surface stays the same vocabulary the dsh plugin's picker uses.
func planFilterAlias(id string) string {
	switch id {
	case "":
		return ""
	case "go":
		return "individual-go"
	case "goat":
		return "individual-goat"
	case "pro":
		return "individual-pro"
	case "provider":
		return "individual-provider"
	case "max":
		return "individual-max"
	default:
		return id
	}
}

// expandEnv replaces ${NAME} (and $NAME) with the process environment.
func expandEnv(value string) string {
	if !strings.Contains(value, "$") {
		return value
	}
	return os.Expand(value, func(name string) string {
		return strings.TrimSpace(os.Getenv(name))
	})
}

// clientModelID maps a catalog id to the client-visible id honoring the
// configured prefix.
func (c config) clientModelID(catalogID string) string {
	if c.ModelPrefix.Enabled == nil || !*c.ModelPrefix.Enabled {
		return catalogID
	}
	return c.ModelPrefix.Value + "/" + catalogID
}

// catalogModelID strips the configured prefix back off a client-visible id.
// It returns ("", false) when the id does not belong to this plugin's
// namespace and should fall through to other providers.
func (c config) catalogModelID(clientID string) (string, bool) {
	clientID = strings.TrimSpace(clientID)
	if c.ModelPrefix.Enabled != nil && *c.ModelPrefix.Enabled {
		prefix := c.ModelPrefix.Value + "/"
		if strings.HasPrefix(clientID, prefix) {
			return strings.TrimPrefix(clientID, prefix), true
		}
		// A prefixed id that is not ours never belongs to this plugin.
		if strings.Contains(c.ModelPrefix.Value, "/") {
			return "", false
		}
	}
	return clientID, true
}
