package plugin

// Shared constants and plan-tier tables for the CommandCode provider port.
//
// The plan tier of every model and every subscription is read from the
// upstream catalog's `minimumPlan` field (GET {base}/models?view=client)
// and from `/alpha/billing/subscriptions` respectively, so no vendored
// snapshot is needed for routing. The tables below exist only for the quota
// page's display (plan label and monthly credit allowance) and are the same
// figures the official CommandCode CLI publishes.

const (
	// ProviderID is the single provider key served by this plugin. It must
	// match the auth record provider key so the host can select a credential.
	ProviderID = "commandcode"

	// PluginName is the plugin ID (dynamic-library file name) reported in
	// registration metadata.
	PluginName = "commandcode-provider"

	// PluginVersion is the release version reported in registration metadata.
	PluginVersion = "0.1.0"

	// GitHubRepository is reported in registration metadata; the plugin store
	// polls it for releases.
	GitHubRepository = "https://github.com/RNGCHEER/cpa-plugin-commandcode-provider"

	// DefaultBaseURL is the CommandCode OpenAI-compatible provider surface.
	DefaultBaseURL = "https://api.commandcode.ai/provider/v1"
)

// planTier orders the plan tiers cheapest first: Go -> GOAT -> Pro -> Provider -> Max.
// A model whose minimum plan has a lower or equal weight is servable by a
// subscription whose tier weight is at least the model's weight.
var planTier = map[string]int{
	"individual-go":       0,
	"individual-go-v1":    0,
	"individual-goat":     1,
	"individual-pro":      2,
	"individual-pro-v1":   2,
	"individual-provider": 3,
	"individual-max":      4,
	"individual-ultra":    4,
	"teams-pro":           2,
}

// planLabel renders a subscription plan id for the quota page. Longest-prefix
// wins, so "individual-go-v1" resolves before "individual-go".
var planLabel = map[string]string{
	"individual-go":       "Go",
	"individual-go-v1":    "Go",
	"individual-goat":     "GOAT",
	"individual-pro":      "Pro",
	"individual-pro-v1":   "Pro",
	"individual-provider": "Provider",
	"individual-max":      "Max",
	"individual-ultra":    "Ultra",
	"teams-pro":           "Teams Pro",
}

// planAllowance is the plan's monthly credit allowance in USD (vendor table).
// An unknown plan simply shows the remaining credits without a denominator.
var planAllowance = map[string]float64{
	"individual-go":       10,
	"individual-go-v1":    10,
	"individual-goat":     70,
	"individual-pro":      30,
	"individual-pro-v1":   80,
	"individual-provider": 15,
	"individual-max":      150,
	"individual-ultra":    300,
	"teams-pro":           40,
}

// planWeight resolves a plan id to its tier weight using longest-prefix match.
// An unknown plan reports -1 so callers can treat it as "no tier known".
func planWeight(planID string) int {
	id := normalizePlanID(planID)
	if id == "" {
		return -1
	}
	if w, ok := planTier[id]; ok {
		return w
	}
	best, bestLen := -1, 0
	for key, weight := range planTier {
		if len(key) > bestLen && len(id) >= len(key) && id[:len(key)] == key && (len(id) == len(key) || id[len(key)] == '-') {
			best, bestLen = weight, len(key)
		}
	}
	return best
}

// planInfo resolves a plan id to its display name and monthly allowance.
func planInfo(planID string) (string, float64) {
	id := normalizePlanID(planID)
	if id == "" {
		return "", 0
	}
	if name, ok := planLabel[id]; ok {
		return name, planAllowance[id]
	}
	best, bestLen := "", 0
	for key := range planLabel {
		if len(key) > bestLen && len(id) >= len(key) && id[:len(key)] == key && (len(id) == len(key) || id[len(key)] == '-') {
			best, bestLen = key, len(key)
		}
	}
	if best == "" {
		return "", 0
	}
	return planLabel[best], planAllowance[best]
}

// normalizePlanID lower-cases a plan id and folds underscores to dashes, the
// same normalization the official CLI applies before its prefix lookups.
func normalizePlanID(planID string) string {
	out := make([]byte, 0, len(planID))
	for i := 0; i < len(planID); i++ {
		c := planID[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c == '_':
			out = append(out, '-')
		default:
			out = append(out, c)
		}
	}
	trimmed := string(out)
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t') {
		trimmed = trimmed[1:]
	}
	for len(trimmed) > 0 && (trimmed[len(trimmed)-1] == ' ' || trimmed[len(trimmed)-1] == '\t') {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return trimmed
}
