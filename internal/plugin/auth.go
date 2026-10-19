package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// authStorage is the plugin-owned credential persisted in the auth file's
// StorageJSON. It carries only what this plugin needs: the provider API key.
type authStorage struct {
	Type   string `json:"type"`
	APIKey string `json:"api_key"`
	Label  string `json:"label,omitempty"`
}

// authFileCandidate reports whether a discovered auth file belongs to this
// plugin: its JSON carries our type marker, or it is the official CLI's
// login file (${HOME}/.commandcode/auth.json) discovered by path.
func authFileCandidate(fileName, path string, raw []byte) bool {
	var probe struct {
		Type      string `json:"type"`
		APIKey    string `json:"api_key"`
		ApiKeyAlt string `json:"apiKey"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		if probe.Type == ProviderID || probe.APIKey != "" || probe.ApiKeyAlt != "" {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(path))
	return base == "auth.json" && strings.Contains(strings.ToLower(path), string(filepath.Separator)+".commandcode"+string(filepath.Separator))
}

// parseAuth recognizes this plugin's own auth files plus the official CLI's
// login file, so a key produced by `cmd login` works without re-entry.
func parseAuth(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	raw := req.RawJSON
	if len(raw) == 0 && req.Path != "" {
		if data, err := os.ReadFile(req.Path); err == nil {
			raw = data
		}
	}
	if len(raw) == 0 || !authFileCandidate(req.FileName, req.Path, raw) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	storage := authStorage{Type: ProviderID}
	if err := json.Unmarshal(raw, &storage); err == nil && storage.Type == "" {
		storage.Type = ProviderID
	}
	// The official CLI's auth.json uses {"apiKey": "..."} (or {"token": "..."}).
	if storage.APIKey == "" {
		var official struct {
			APIKey string `json:"apiKey"`
			Token  string `json:"token"`
		}
		if err := json.Unmarshal(raw, &official); err == nil {
			storage.APIKey = strings.TrimSpace(official.APIKey)
			if storage.APIKey == "" {
				storage.APIKey = strings.TrimSpace(official.Token)
			}
		}
	}
	storage.APIKey = strings.TrimSpace(storage.APIKey)
	if storage.APIKey == "" {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	storage.Type = ProviderID

	id := authID(req.FileName, storage.APIKey)
	label := strings.TrimSpace(storage.Label)
	if label == "" {
		label = "CommandCode"
		if name := strings.TrimSuffix(filepath.Base(req.FileName), filepath.Ext(req.FileName)); name != "" && name != "auth" {
			label = name
		}
	}
	stored, err := json.Marshal(storage)
	if err != nil {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("encode auth storage: %w", err)
	}
	auth := pluginapi.AuthData{
		Provider:    ProviderID,
		ID:          id,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: stored,
		Metadata: map[string]any{
			"type": ProviderID,
		},
		Attributes: map[string]string{
			"api_key": storage.APIKey,
		},
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// refreshAuth re-parses the persisted storage and reports the next refresh as
// "never" - CommandCode keys are long-lived and the upstream exposes no token
// to rotate. The quota page refreshes account state on demand instead.
func refreshAuth(req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	var storage authStorage
	if len(req.StorageJSON) > 0 {
		if err := json.Unmarshal(req.StorageJSON, &storage); err != nil {
			return pluginapi.AuthRefreshResponse{}, fmt.Errorf("decode auth storage: %w", err)
		}
	}
	if strings.TrimSpace(storage.APIKey) == "" {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("commandcode auth carries no api key")
	}
	storage.Type = ProviderID
	stored, err := json.Marshal(storage)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("encode auth storage: %w", err)
	}
	id := authID(req.AuthID, storage.APIKey)
	label := strings.TrimSpace(storage.Label)
	if label == "" {
		label = "CommandCode"
	}
	return pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    ProviderID,
			ID:          id,
			FileName:    req.AuthID,
			Label:       label,
			StorageJSON: stored,
			Metadata:    req.Metadata,
			Attributes:  map[string]string{"api_key": strings.TrimSpace(storage.APIKey)},
		},
		// Zero time means "no scheduled refresh" for the host.
		NextRefreshAfter: time.Time{},
	}, nil
}

// authID derives a stable auth record id from the file name (or the key when
// no file name exists), so re-parses of the same file keep one record.
func authID(fileName, key string) string {
	seed := strings.TrimSpace(fileName)
	if seed == "" {
		seed = strings.TrimSpace(key)
	}
	if seed == "" {
		return ProviderID + "-auth"
	}
	// Keep it filesystem- and YAML-friendly: the host persists this as the
	// auth file stem when it writes the record back.
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r == '.':
			return '-'
		default:
			return '-'
		}
	}, seed)
	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" {
		return ProviderID + "-auth"
	}
	return ProviderID + "-" + sanitized
}

// loginStart / loginPoll: the official sign-in flow is a browser loopback
// dance owned by the vendor CLI. This port takes the key from config or an
// auth file instead, so login is reported as unsupported rather than faked.
func startLogin(_ pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("commandcode: browser sign-in is not ported; set api-keys in plugins.configs.%s or drop an auth file", PluginName)
}

func pollLogin(_ pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "commandcode: browser sign-in is not ported"}, nil
}

// ensure the context import is used even when build tags change the login
// bodies above.
var _ = context.Background
var _ = http.StatusOK
