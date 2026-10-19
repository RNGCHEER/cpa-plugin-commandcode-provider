package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// registrationResult is the plugin.register / plugin.reconfigure payload.
type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// capabilities declares only what this plugin implements.
type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	ManagementAPI         bool                         `json:"management_api"`
}

// registrationEnvelope renders the register/reconfigure answer. Every format
// the host may hand us is translated to the one upstream transport this port
// speaks (OpenAI chat-completions), so the executor declares all three on
// both edges.
func registrationEnvelope() ([]byte, error) {
	formats := []string{"openai", "claude", "openai-response"}
	result := registrationResult{
		SchemaVersion: 6,
		Metadata: pluginapi.Metadata{
			Name:             PluginName,
			Version:          PluginVersion,
			Author:           "RNGCHEER",
			GitHubRepository: GitHubRepository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable this plugin (host also honors plugins.configs." + PluginName + ".enabled)."},
				{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Default: DefaultBaseURL, Description: "CommandCode provider base URL (https://api.commandcode.ai/provider/v1)."},
				{Name: "catalog-url", Type: pluginapi.ConfigFieldTypeString, Description: "Model catalog URL override (default {base-url}/models)."},
				{Name: "api-keys", Type: pluginapi.ConfigFieldTypeArray, Description: "Ordered CommandCode API key pool; supports ${ENV_VAR}. The selected auth record is tried first, then this pool."},
				{Name: "plan-filter", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"", "go", "goat", "pro", "provider", "max"}, Description: "Hide models whose minimum plan is above this tier (empty keeps all)."},
				{Name: "visible-models", Type: pluginapi.ConfigFieldTypeArray, Description: "Allowlist of catalog model ids to publish (empty keeps all)."},
				{Name: "exclude-hidden", Type: pluginapi.ConfigFieldTypeBoolean, Default: true, Description: "Drop the catalog's hidden model rows."},
				{Name: "request-timeout", Type: pluginapi.ConfigFieldTypeString, Default: "5m", Description: "Per-attempt upstream request timeout."},
				{Name: "allow-http", Type: pluginapi.ConfigFieldTypeBoolean, Default: false, Description: "Permit http:// base URLs (local testing only)."},
				{Name: "model-prefix", Type: pluginapi.ConfigFieldTypeObject, Description: "Client-visible model id prefix (enabled/value; default commandcode)."},
			},
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         true,
		},
	}
	return okEnvelope(result)
}

// handleModels serves model.static and model.for_auth. Both return the same
// filtered catalog: the plugin owns one upstream, so per-credential discovery
// adds nothing beyond what the static list already carries.
func (m *Manager) handleModels(_ []byte) ([]byte, error) {
	cfg, client := m.snapshot()
	rows := m.catalog.snapshot()
	if len(rows) == 0 {
		// First call (or restart): fetch once so /v1/models is warm; on
		// failure an empty list is a valid "not ready yet" answer and the
		// background loop will retry.
		fetched, err := m.catalog.fetchCatalog(context.Background(), cfg, client)
		if err != nil {
			m.bridge.Log("warn", "commandcode: model catalog unavailable", map[string]any{"reason": err.Error()})
		}
		rows = fetched
	}
	visible := cfg.visible(rows)
	models := make([]pluginapi.ModelInfo, 0, len(visible))
	for _, row := range visible {
		info := pluginapi.ModelInfo{
			ID:                         cfg.clientModelID(row.ID),
			Object:                     "model",
			OwnedBy:                    ProviderID,
			DisplayName:                displayName(row),
			ContextLength:              row.ContextLength,
			SupportedGenerationMethods: []string{"chat", "completions", "generate"},
			UserDefined:                false,
		}
		if row.MaxOutputTokens > 0 {
			info.MaxCompletionTokens = row.MaxOutputTokens
		}
		if len(row.ReasoningEfforts) > 0 {
			info.Thinking = &pluginapi.ThinkingSupport{Levels: append([]string(nil), row.ReasoningEfforts...)}
		}
		if supportsImage(row) {
			info.SupportedInputModalities = append([]string(nil), row.InputModalities...)
		}
		models = append(models, info)
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models})
}

// displayName prefers the catalog's display label, then the row name, then
// the raw id.
func displayName(row catalogModel) string {
	if label := strings.TrimSpace(row.Display.Label); label != "" {
		return label
	}
	if name := strings.TrimSpace(row.Name); name != "" {
		return name
	}
	return row.ID
}

// supportsImage reports whether the catalog row advertises image input.
func supportsImage(row catalogModel) bool {
	for _, modality := range row.InputModalities {
		if modality == "image" {
			return true
		}
	}
	return false
}

// ---- management (quota page) -------------------------------------------------

// managementRegistration declares the plugin's own routes and resource page.
func managementRegistration() ([]byte, error) {
	return okEnvelope(struct {
		Routes    []pluginapi.ManagementRoute `json:"routes"`
		Resources []pluginapi.ResourceRoute   `json:"resources"`
	}{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodPost, Path: "/plugins/" + PluginName + "/quota-usage"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/quota", Menu: "CommandCode Quota", Description: "View CommandCode plan and usage windows."},
		},
	})
}

// handleManagement serves the quota page (resource) and its data endpoint
// (management route). Data requests are forwarded to the host's auth list so
// every credential can be shown without the page knowing file paths.
func (m *Manager) handleManagement(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method == http.MethodGet && (strings.HasSuffix(req.Path, "/quota") || strings.HasSuffix(req.Path, "/quota/")) {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       []byte(quotaPageHTML),
		}, nil
	}
	if !strings.HasSuffix(req.Path, "/quota-usage") {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte("{\"error\":\"not found\"}")}, nil
	}
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	return m.quotaUsage(context.Background(), strings.TrimSpace(body.AuthIndex))
}

// quotaUsage renders one credential's account snapshot (or the list of all
// credentials when no index is given). Credentials come from TWO sources so
// the page never shows "no credentials" when only api-keys are configured:
// the plugin's own api-keys pool (config) and the host's auth records
// (auths/ files). Both are enumerated; each card's id addresses either a
// config key (key_id, a non-secret sha256 digest) or an auth record
// (auth_index). Account state is read through the host HTTP client, so keys
// never appear in the page's own transport.
func (m *Manager) quotaUsage(ctx context.Context, target string) (pluginapi.ManagementResponse, error) {
	cfg, client := m.snapshot()
	entries, err := m.hostAuthList(ctx)
	if err != nil {
		entries = nil // host auth list unavailable: still serve config keys
	}
	if target == "" {
		return managementJSON(http.StatusOK, map[string]any{"cards": m.quotaCards(cfg, entries)}), nil
	}
	// Resolve the target: a config key digest first, then an auth record.
	if key := quotaKeyByHash(cfg, target); key != "" {
		usage, label, email, errUsage := fetchQuota(ctx, client, cfg, key)
		if errUsage != nil {
			return managementJSON(http.StatusOK, map[string]any{"key_id": target, "label": "CommandCode credential " + target[:12], "error": errUsage.Error()}), nil
		}
		_ = label
		return managementJSON(http.StatusOK, map[string]any{"key_id": target, "label": label, "email": email, "usage": usage}), nil
	}
	entry, ok := findAuthEntry(entries, target)
	if !ok {
		return managementJSON(http.StatusNotFound, map[string]any{"error": "unknown credential"}), nil
	}
	key, err := m.hostAuthKey(ctx, target)
	if err != nil || key == "" {
		return managementJSON(http.StatusOK, map[string]any{"auth_index": target, "error": "credential key unavailable"}), nil
	}
	usage, label, email, errUsage := fetchQuota(ctx, client, cfg, key)
	if errUsage != nil {
		return managementJSON(http.StatusOK, map[string]any{"auth_index": target, "label": entry.Label, "error": errUsage.Error()}), nil
	}
	_ = label
	return managementJSON(http.StatusOK, map[string]any{
		"auth_index": target,
		"label":      firstNonEmpty(entry.Label, entry.Name),
		"email":      firstNonEmpty(email, entry.Email),
		"usage":      usage,
	}), nil
}

// quotaCards merges the two credential sources into the page's card list.
// Config keys are listed first (keyed by digest) because that is the
// documented primary setup; auth records follow (keyed by auth_index). A key
// that also has an auth record is deduped by its digest.
func (m *Manager) quotaCards(cfg config, entries []pluginapi.HostAuthFileEntry) []map[string]any {
	seen := map[string]bool{}
	cards := make([]map[string]any, 0, len(cfg.APIKeys)+len(entries))
	for _, key := range cfg.APIKeys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		hash := authKeyHash(key)
		cards = append(cards, map[string]any{
			"key_id": hash,
			"label":  "CommandCode credential " + hash[:12],
		})
	}
	for _, entry := range entries {
		if entry.Provider != "" && entry.Provider != ProviderID {
			continue
		}
		cards = append(cards, map[string]any{
			"auth_index": entry.AuthIndex,
			"name":       entry.Name,
			"label":      entry.Label,
			"email":      entry.Email,
		})
	}
	return cards
}

// quotaKeyByHash resolves a key digest (target) back to its configured api-key.
func quotaKeyByHash(cfg config, target string) string {
	target = strings.ToLower(strings.TrimSpace(target))
	for _, key := range cfg.APIKeys {
		if authKeyHash(strings.TrimSpace(key)) == target {
			return strings.TrimSpace(key)
		}
	}
	return ""
}

func findAuthEntry(entries []pluginapi.HostAuthFileEntry, index string) (pluginapi.HostAuthFileEntry, bool) {
	for _, entry := range entries {
		if entry.AuthIndex == index || entry.ID == index || entry.Name == index {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func managementJSON(status int, payload any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{\"error\":\"encode failed\"}")
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}
}

// quotaWindow is one usage window as the page renders it.
type quotaWindow struct {
	Status   string  `json:"status"`
	Percent  int     `json:"percent"`
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	ResetsAt string  `json:"resets_at"`
}

// quotaUsagePayload is the per-credential snapshot the page polls.
type quotaUsagePayload struct {
	Plan             string       `json:"plan"`
	PlanCredits      float64      `json:"plan_credits"`
	CreditsLeft      float64      `json:"credits_left"`
	PurchasedCredits float64      `json:"purchased_credits"`
	Month            *quotaWindow `json:"month"`
	FiveHour         quotaWindow  `json:"five_hour"`
	Weekly           quotaWindow  `json:"weekly"`
	RefreshedAt      string       `json:"refreshed_at"`
}

// ---- account API shapes (probed live against api.commandcode.ai) -----------

type accountCredits struct {
	Credits struct {
		MonthlyCredits   float64 `json:"monthlyCredits"`
		PurchasedCredits float64 `json:"purchasedCredits"`
		FreeCredits      float64 `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits struct {
		Limited  bool          `json:"limited"`
		FiveHour accountWindow `json:"fiveHour"`
		Weekly   accountWindow `json:"weekly"`
	} `json:"windowLimits"`
}

type accountWindow struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded"`
	ResetAt  int64   `json:"resetAt"`
}

type accountSubscription struct {
	Success bool `json:"success"`
	Data    struct {
		PlanID           string `json:"planId"`
		Status           string `json:"status"`
		CurrentPeriodEnd string `json:"currentPeriodEnd"`
	} `json:"data"`
}

type accountWhoami struct {
	Success bool `json:"success"`
	User    struct {
		Email string `json:"email"`
	} `json:"user"`
	Org *struct {
		Login string `json:"login"`
	} `json:"org"`
}

// accountAPIBase derives <scheme>://<authority> from the provider base URL by
// trimming the /provider/v1 surface, so the account API (<authority>/alpha/*)
// needs no second config key.
func accountAPIBase(baseURL string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/provider/v1")
	trimmed = strings.TrimSuffix(trimmed, "/provider")
	if !strings.HasPrefix(trimmed, "https://") && !strings.HasPrefix(trimmed, "http://") {
		return "", fmt.Errorf("base-url is not an absolute URL")
	}
	return trimmed, nil
}

// fetchQuota reads one key's plan, credits and windows from the account API.
// Plan identity comes from /alpha/billing/subscriptions, credits and windows
// from /alpha/billing/credits, and the mailbox from /alpha/whoami.
func fetchQuota(ctx context.Context, client HostHTTPDoer, cfg config, key string) (quotaUsagePayload, string, string, error) {
	if client == nil {
		return quotaUsagePayload{}, "", "", fmt.Errorf("host HTTP client unavailable")
	}
	base, err := accountAPIBase(cfg.BaseURL)
	if err != nil {
		return quotaUsagePayload{}, "", "", err
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	get := func(path string) ([]byte, error) {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		resp, errDo := client.Do(reqCtx, pluginapi.HTTPRequest{
			Method: http.MethodGet,
			URL:    base + path,
			Headers: http.Header{
				"Authorization": []string{"Bearer " + key},
				"Accept":        []string{"application/json"},
			},
		})
		if errDo != nil {
			return nil, fmt.Errorf("account request failed")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("account request rejected (%d)", resp.StatusCode)
		}
		return resp.Body, nil
	}

	rawCredits, err := get("/alpha/billing/credits")
	if err != nil {
		return quotaUsagePayload{}, "", "", err
	}
	var credits accountCredits
	if err := json.Unmarshal(rawCredits, &credits); err != nil {
		return quotaUsagePayload{}, "", "", fmt.Errorf("account credits response invalid: %w", err)
	}
	usage := quotaUsagePayload{
		CreditsLeft:      credits.Credits.MonthlyCredits,
		PurchasedCredits: credits.Credits.PurchasedCredits,
		FiveHour:         windowFromAccount(credits.WindowLimits.FiveHour, credits.WindowLimits.Limited),
		Weekly:           windowFromAccount(credits.WindowLimits.Weekly, credits.WindowLimits.Limited),
		RefreshedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	label, email := "", ""
	if raw, errSub := get("/alpha/billing/subscriptions"); errSub == nil {
		var sub accountSubscription
		if json.Unmarshal(raw, &sub) == nil && sub.Success {
			if name, allowance := planInfo(sub.Data.PlanID); name != "" {
				usage.Plan = name
				usage.PlanCredits = allowance
			}
		}
	}
	usage.Month = monthWindow(usage.PlanCredits, usage.CreditsLeft, usage.PurchasedCredits)
	if raw, errWho := get("/alpha/whoami?limits=1"); errWho == nil {
		var who accountWhoami
		if json.Unmarshal(raw, &who) == nil {
			email = strings.TrimSpace(who.User.Email)
			label = email
			if who.Org != nil && strings.TrimSpace(who.Org.Login) != "" {
				label = strings.TrimSpace(who.Org.Login) + " (" + email + ")"
			}
		}
	}
	return usage, label, email, nil
}

func windowFromAccount(w accountWindow, limited bool) quotaWindow {
	out := quotaWindow{Used: w.Used, Cap: w.Cap, Status: "ok"}
	switch {
	case w.Exceeded:
		out.Status = "exceeded"
	case !limited:
		out.Status = "unlimited"
	}
	if w.Cap > 0 {
		percent := int(w.Used / w.Cap * 100)
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
		out.Percent = percent
	}
	if w.ResetAt > 0 {
		out.ResetsAt = time.UnixMilli(w.ResetAt).UTC().Format(time.RFC3339)
	}
	return out
}

// monthWindow renders the plan's monthly credits as a window. The account API
// reports only the REMAINING monthly credits, so the pool is derived the way
// the vendor CLI's own bar does: pool = allowance + purchased. An unknown plan
// has no allowance, so no window is shown (a bar without a denominator would
// be a lie).
func monthWindow(planCredits, creditsLeft, purchased float64) *quotaWindow {
	if planCredits <= 0 {
		return nil
	}
	remaining := creditsLeft + purchased
	pool := planCredits + purchased
	if pool <= remaining {
		pool = remaining
	}
	if pool <= 0 {
		return nil
	}
	used := pool - remaining
	if used < 0 {
		used = 0
	}
	out := &quotaWindow{Used: used, Cap: pool, Status: "ok"}
	if remaining <= 0 {
		out.Status = "exceeded"
	}
	percent := int(used / pool * 100)
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	out.Percent = percent
	return out
}
