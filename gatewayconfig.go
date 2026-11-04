package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

const (
	defaultAPIPort = 8317
	defaultWebPort = 8316
)

// Keys are persisted in plaintext as configured by the user; APIKeyEnabled only affects the model API and must never disable admin authentication.
type gatewayFileConfig struct {
	APIPort       int    `json:"apiPort"`
	WebPort       int    `json:"webPort"`
	AdminKey      string `json:"adminKey"`
	APIKeyEnabled bool   `json:"apiKeyEnabled"`
	APIKey        string `json:"apiKey"`
}

type gatewayValidationError string

func (e gatewayValidationError) Error() string { return string(e) }

var (
	gatewayConfigMu sync.RWMutex // serializes config writes and runtime Key switches; HTTP handlers are called without the lock
	gatewaySettings gatewayFileConfig
	errSetupDone    = errors.New("admin Key already configured; after external changes wait for config hot reload and log in again")
	errConfigChange = errors.New("admin Key was changed externally; wait for config hot reload and log in again with the new Key")
)

func normalizeGatewayConfig(g gatewayFileConfig) (gatewayFileConfig, error) {
	if g.APIPort == 0 {
		g.APIPort = defaultAPIPort
	}
	if g.WebPort == 0 {
		g.WebPort = defaultWebPort
	}
	if g.APIPort < 1 || g.APIPort > 65535 || g.WebPort < 1 || g.WebPort > 65535 {
		return g, gatewayValidationError("gateway.apiPort/webPort must be 1–65535; omitted or 0 uses the default")
	}
	if g.APIPort == g.WebPort {
		return g, gatewayValidationError("API port and Web port cannot be the same")
	}
	for name, value := range map[string]string{"adminKey": g.AdminKey, "apiKey": g.APIKey} {
		for _, c := range value {
			if c < 33 || c > 126 {
				return g, gatewayValidationError(fmt.Sprintf("gateway.%s can only contain non-whitespace printable ASCII characters", name))
			}
		}
	}
	if g.APIKeyEnabled && g.APIKey == "" {
		return g, gatewayValidationError("gateway.apiKey must be set when API Key validation is enabled")
	}
	return g, nil
}

func applyGatewayConfig(g gatewayFileConfig) error {
	g, err := normalizeGatewayConfig(g)
	if err != nil {
		return err
	}
	apiPort := g.APIPort
	if cfg.PortExplicit {
		apiPort = cfg.Port
	}
	if apiPort < 1 || apiPort > 65535 || cfg.WebUI && apiPort == g.WebPort {
		return errors.New("API listen port invalid or conflicts with the Web listen port")
	}
	gatewayConfigMu.Lock()
	defer gatewayConfigMu.Unlock()
	gatewaySettings = g
	cfg.Port, cfg.WebPort = apiPort, g.WebPort
	setGatewayKeysLocked(g)
	return nil
}

func setGatewayKeysLocked(g gatewayFileConfig) {
	registerSecrets(g.AdminKey, g.APIKey)
	cfg.AdminKey = g.AdminKey
	cfg.APIKey = ""
	if g.APIKeyEnabled {
		cfg.APIKey = g.APIKey
	}
}

func currentAPIKey() string {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	return cfg.APIKey
}

func currentAdminKey() string {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	return cfg.AdminKey
}

// readGatewayDocumentLocked preserves other config sections; it never rebuilds prompts/account rules as defaults.
func readGatewayDocumentLocked() (map[string]json.RawMessage, gatewayFileConfig, []byte, error) {
	data, err := os.ReadFile(runtimeConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		g, _ := normalizeGatewayConfig(gatewayFileConfig{})
		return map[string]json.RawMessage{}, g, nil, nil
	}
	if err != nil {
		return nil, gatewayFileConfig{}, nil, err
	}
	typed, err := parseRuntimeConfig(data)
	if err != nil {
		return nil, gatewayFileConfig{}, nil, fmt.Errorf("config file unparseable, refusing to overwrite: %w", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		return nil, gatewayFileConfig{}, nil, errors.New("config.json must be a JSON object")
	}
	g, err := normalizeGatewayConfig(typed.Gateway)
	return doc, g, data, err
}

// writeConfigPreservingFile writes in place to preserve existing file ACLs; on failure it best-effort restores the original text.
// It does not create temporary replacement files containing secrets and does not claim power-failure transactional guarantees.
func writeConfigPreservingFile(data, previous []byte) error {
	f, err := os.OpenFile(runtimeConfigFile, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	write := func(content []byte) error {
		n, err := f.WriteAt(content, 0)
		if err == nil && n != len(content) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = f.Truncate(int64(len(content)))
		}
		if err == nil {
			err = f.Sync()
		}
		return err
	}
	if err := write(data); err != nil {
		if previous != nil {
			if restoreErr := write(previous); restoreErr != nil {
				return fmt.Errorf("config write failed: %v; restoring the original file also failed, please check the file: %w", err, restoreErr)
			}
		}
		return err
	}
	return nil
}

func persistGatewayConfigLocked(doc map[string]json.RawMessage, g gatewayFileConfig, old []byte, trace string) error {
	raw, err := json.Marshal(g)
	if err != nil {
		return err
	}
	doc["gateway"] = raw
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	log.Printf("[Admin config] traceId=%s stage=write-config file=%s api-auth-enabled=%t key-hidden=true note=persist to disk before switching runtime config; other config sections preserved", trace, runtimeConfigFile, g.APIKeyEnabled)
	if err := writeConfigPreservingFile(append(data, '\n'), old); err != nil {
		log.Printf("[Admin config] traceId=%s stage=write-config result=failed reason=%v business-impact=runtime config not switched", trace, err)
		return err
	}
	if err := applyGatewayListenerPortsLocked(g, trace); err != nil {
		if restoreErr := writeConfigPreservingFile(old, append(data, '\n')); restoreErr != nil {
			return fmt.Errorf("listen port switch failed: %v; restoring the original config also failed, please check the server logs: %w", err, restoreErr)
		}
		return err
	}
	gatewaySettings = g
	setGatewayKeysLocked(g)
	// This function only applies the gateway section; force a full reload next round for other externally edited config.
	runtimeHasAppliedHash = false
	log.Printf("[Admin config] traceId=%s result=save-success api-auth-enabled=%t actual-api-port=%d actual-web-port=%d note=following requests use the new auth state; admin auth always stays independently enabled; listen ports hot-switched, old connections retained", trace, g.APIKeyEnabled, cfg.Port, cfg.WebPort)
	return nil
}

// Called with gatewayConfigMu held. New listeners bind before old ones retire.
func applyGatewayListenerPortsLocked(g gatewayFileConfig, trace string, beforeCommit ...func() error) error {
	apiPort := g.APIPort
	if cfg.PortExplicit {
		apiPort = cfg.Port
	}
	var prepare func() error
	if len(beforeCommit) > 0 {
		prepare = beforeCommit[0]
	}
	actualAPI, actualWeb, err := gatewayServers.applyWith(g, apiPort, cfg.WebUI, prepare)
	if err != nil {
		log.Printf("[Listener hot switch] traceId=%s target-api-port=%d target-web-port=%d result=failed reason=%v business-impact=original listener keeps serving", trace, apiPort, g.WebPort, err)
		return gatewayValidationError(err.Error())
	}
	if actualAPI > 0 {
		cfg.Port = actualAPI
	}
	if actualWeb > 0 {
		cfg.WebPort = actualWeb
	}
	return nil
}

func currentGatewayPorts() (int, int) {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	api, web, _ := gatewayServers.ports()
	return api, web
}

func initializeAdminKey(trace string) (string, error) {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	gatewayConfigMu.Lock()
	defer gatewayConfigMu.Unlock()
	if cfg.AdminKey != "" {
		return "", errSetupDone
	}
	doc, g, old, err := readGatewayDocumentLocked()
	if err != nil {
		return "", err
	}
	if g.AdminKey != "" {
		return "", errSetupDone
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("failed to generate admin Key: %w", err)
	}
	g.AdminKey = hex.EncodeToString(entropy[:]) // exactly 32 characters, 128 bits of random entropy.
	registerSecrets(g.AdminKey)
	if err := persistGatewayConfigLocked(doc, g, old, trace); err != nil {
		return "", err
	}
	return g.AdminKey, nil
}

func saveAPIKeySettings(enabled bool, key, trace string) error {
	return saveGatewaySettings(&enabled, key, nil, nil, trace)
}

func saveGatewaySettings(enabled *bool, key string, apiPort, webPort *int, trace string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	gatewayConfigMu.Lock()
	defer gatewayConfigMu.Unlock()
	doc, g, old, err := readGatewayDocumentLocked()
	if err != nil {
		return err
	}
	if cfg.AdminKey == "" || g.AdminKey != cfg.AdminKey {
		return errConfigChange
	}
	if enabled != nil {
		g.APIKeyEnabled = *enabled
	}
	if apiPort != nil {
		g.APIPort = *apiPort
	}
	if webPort != nil {
		g.WebPort = *webPort
	}
	// Empty input means keep the configured Key; disabling validation does not delete the original Key.
	if key != "" {
		g.APIKey = key
	}
	g, err = normalizeGatewayConfig(g)
	if err != nil {
		return err
	}
	effectiveAPIPort := g.APIPort
	if cfg.PortExplicit {
		effectiveAPIPort = cfg.Port
	}
	if cfg.WebUI && effectiveAPIPort == g.WebPort {
		return gatewayValidationError("Web port cannot conflict with the actual API port (including the -port command-line override)")
	}
	log.Printf("[Admin port settings] traceId=%s configured-api-port=%d configured-web-port=%d current-api-port=%d current-web-port=%d note=saving is equivalent to editing the gateway config; listen ports hot-switch after validation",
		trace, g.APIPort, g.WebPort, cfg.Port, cfg.WebPort)
	registerSecrets(g.APIKey)
	return persistGatewayConfigLocked(doc, g, old, trace)
}

func gatewaySettingsView() map[string]any {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	actualAPI, actualWeb, live := gatewayServers.ports()
	if !live {
		actualAPI, actualWeb = cfg.Port, cfg.WebPort
	}
	return map[string]any{
		"apiKeyEnabled":      cfg.APIKey != "",
		"apiKeyConfigured":   gatewaySettings.APIKey != "",
		"adminKeyConfigured": cfg.AdminKey != "",
		"apiPort":            actualAPI, "webPort": actualWeb,
		"configuredApiPort": gatewaySettings.APIPort,
		"configuredWebPort": gatewaySettings.WebPort,
		"apiPortOverridden": cfg.PortExplicit,
		"restartRequired":   gatewaySettings.WebPort != actualWeb || !cfg.PortExplicit && gatewaySettings.APIPort != actualAPI,
		"livePortSwitching": live,
	}
}

// ignoreLegacyAPIKeyFlag just swallows a leftover -api-key from old systemd units/scripts, preventing startup failures after upgrades.
//
// In the new version this flag is **completely silent and has no effect**: it writes no logs, writes no files, does not modify config.json,
// and does not affect model API authentication; model API authentication is decided solely by config.json's gateway.apiKeyEnabled / apiKey.
//
// TODO(remove): this is a pure compatibility placeholder, planned for full removal in a later version along with its registration in main.go;
// after removal, old startup commands will fail on the unknown flag, and release notes should then tell users to remove it first.
func ignoreLegacyAPIKeyFlag(_ string) {
}
