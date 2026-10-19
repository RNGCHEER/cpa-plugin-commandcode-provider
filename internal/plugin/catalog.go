package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogModel is one row of the CommandCode model catalog. The upstream
// exposes two views: the bare OpenAI list (id/name/context_length/...) and the
// client view (adds minimumPlan/maxOutputTokens/reasoningEfforts/hidden/zdr/
// inputModalities). The client view carries the plan tier this port needs, so
// it is the primary source and the bare list is the fallback.
type catalogModel struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	ContextLength      int64    `json:"context_length"`
	MaxOutputTokens    int64    `json:"maxOutputTokens"`
	MinimumPlan        string   `json:"minimumPlan"`
	ReasoningEfforts   []string `json:"reasoningEfforts"`
	InputModalities    []string `json:"inputModalities"`
	ZDR                bool     `json:"zdr"`
	Hidden             bool     `json:"hidden"`
	SupportedEndpoints []string `json:"supported_endpoints"`
	Display            struct {
		Label       string `json:"label"`
		Description string `json:"description"`
		Group       string `json:"group"`
	} `json:"display"`
}

// catalogResponse matches both view shapes ({"object":"list","data":[...]}).
type catalogResponse struct {
	Object string         `json:"object"`
	Data   []catalogModel `json:"data"`
}

// catalogState caches the last successfully fetched catalog.
type catalogState struct {
	mu        sync.RWMutex
	models    []catalogModel
	fetchedAt time.Time
	staleErr  error
}

// fetchCatalog pulls the model list through the host HTTP client so proxy
// policy and request logging stay with the host. It tries the client view
// first (carries minimumPlan) and falls back to the bare view.
func (s *catalogState) fetchCatalog(ctx context.Context, cfg config, client HostHTTPDoer) ([]catalogModel, error) {
	if client == nil {
		return nil, fmt.Errorf("commandcode: host HTTP client unavailable")
	}
	url := strings.TrimSuffix(cfg.CatalogURL, "/")
	if url == "" {
		url = strings.TrimSuffix(cfg.BaseURL, "/") + "/models"
	}
	rows, err := s.fetchView(ctx, url+"?view=client", client, cfg.RequestTimeout)
	if err != nil {
		rows, err = s.fetchView(ctx, url, client, cfg.RequestTimeout)
	}
	if err != nil {
		s.mu.Lock()
		if len(s.models) > 0 {
			// Keep serving the last valid catalog (stale-while-unavailable).
			s.staleErr = err
			s.mu.Unlock()
			return append([]catalogModel(nil), s.models...), nil
		}
		s.staleErr = err
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	s.models = rows
	s.fetchedAt = time.Now()
	s.staleErr = nil
	s.mu.Unlock()
	return append([]catalogModel(nil), rows...), nil
}

func (s *catalogState) fetchView(ctx context.Context, url string, client HostHTTPDoer, timeout time.Duration) ([]catalogModel, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := client.Do(reqCtx, pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    url,
		Headers: http.Header{
			"Accept": []string{"application/json"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("catalog request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog request rejected (%d)", resp.StatusCode)
	}
	var parsed catalogResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return nil, fmt.Errorf("catalog response invalid: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("catalog response empty")
	}
	// Stable order: the upstream list order is already grouped, but sorting by
	// id keeps registration deterministic across refreshes.
	rows := parsed.Data
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// visible returns the rows that should be registered as client-visible models
// after applying the hidden/plan/allowlist filters.
func (c config) visible(rows []catalogModel) []catalogModel {
	filterWeight := -1
	if c.PlanFilter != "" {
		filterWeight = planWeight(c.PlanFilter)
	}
	allow := map[string]bool{}
	for _, id := range c.VisibleModels {
		allow[id] = true
	}
	out := make([]catalogModel, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		if c.ExcludeHidden != nil && *c.ExcludeHidden && row.Hidden {
			continue
		}
		if len(allow) > 0 && !allow[row.ID] {
			continue
		}
		if filterWeight >= 0 {
			if w := planWeight(row.MinimumPlan); w > filterWeight {
				continue
			}
		}
		out = append(out, row)
	}
	return out
}

// lookup returns the catalog row for a catalog id, or false when unknown.
func (s *catalogState) lookup(id string) (catalogModel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, row := range s.models {
		if row.ID == id {
			return row, true
		}
	}
	return catalogModel{}, false
}

// snapshot returns a copy of the cached rows without touching the network.
func (s *catalogState) snapshot() []catalogModel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]catalogModel(nil), s.models...)
}
