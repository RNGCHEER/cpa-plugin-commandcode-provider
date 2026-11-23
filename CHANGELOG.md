# Changelog

## 0.3.0 — 2026-11-23

- Align inference with pi-commandcode-provider v0.8.7 (upstream commit 7a061352df4172b7bcb72405b518a473bfb8dca7). Requires Provider API entitlement; 403 upgrade_required no longer invokes the private CLI gateway.
- Route Claude models to Messages, prefer Responses when advertised, and use Chat Completions otherwise. Add validated per-model endpoint-overrides.
- Use CPA SDK request/response translators for tools, images and client protocols. Translate Chat Completions to standard Responses without Codex-specific request defaults.
- Import command-code@1.91.1 capability metadata; retain live catalog discovery and cap output budgets by context size.
- Fill missing Go plan metadata from the official 62-model Go page snapshot retrieved on 2026-11-23. Existing live plan metadata wins; snapshot IDs are never added to the advertised catalog.
- Add opt-in enable-zdr and CMD_ZDR / COMMANDCODE_ZDR environment support.
- Apply model-scoped GLM-5.3 and Mistral Large 4 Responses request repairs. Recover GLM-5.3 plaintext reasoning only when no canonical reasoning has already arrived.
- Validate stored credentials against whoami during auth.refresh; revoked keys fail with 401 invalid_token while temporary failures remain distinct.

Migration: existing auth files and key/plan management remain available. Legacy generate-url, cli-version and working-dir values no longer affect inference. Go plan model availability does not grant Provider API entitlement.

Validation: go test ./..., go vet ./..., and shared-library builds in GitHub Actions. Mocked protocol tests cover model routing, tools/images, split SSE chunks, response conversion, ZDR, Go metadata, and entitlement denials. Live inference requires an entitled account and was not exercised by this update.
