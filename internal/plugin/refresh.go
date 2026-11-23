package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"net/http"
)

// A key cannot be renewed locally: validate it before returning it to the host.
// A rejected key and a temporary upstream outage remain distinct errors.
func (m *Manager) refreshProviderAuth(req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	var storage authStorage
	if err := json.Unmarshal(req.StorageJSON, &storage); err != nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("invalid auth storage")
	}
	if storage.APIKey == "" {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("commandcode auth carries no api key")
	}
	cfg := m.snapshotConfig()
	base, err := accountAPIBase(cfg.BaseURL)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
	defer cancel()
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: base + "/alpha/whoami", Headers: providerHeaders(cfg, storage.APIKey, false, "")})
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("commandcode: credential validation temporarily unavailable")
	}
	if resp.StatusCode == 401 {
		return pluginapi.AuthRefreshResponse{}, &upstreamError{status: 401, msg: "401 invalid_token"}
	}
	if resp.StatusCode != 200 {
		return pluginapi.AuthRefreshResponse{}, &upstreamError{status: resp.StatusCode, msg: "commandcode: credential validation rejected"}
	}
	return refreshAuth(req)
}
