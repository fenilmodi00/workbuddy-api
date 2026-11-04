package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	// Cross-compiled test binaries may run without their original build path.
	// The deployment preflight supplies a private, extracted source snapshot.
	if root := os.Getenv("WORKBUDDY_TEST_SOURCE_ROOT"); root != "" {
		if !filepath.IsAbs(root) {
			t.Fatal("WORKBUDDY_TEST_SOURCE_ROOT must be absolute")
		}
		if info, err := os.Stat(filepath.Join(root, "go.mod")); err != nil || !info.Mode().IsRegular() {
			t.Fatal("WORKBUDDY_TEST_SOURCE_ROOT must contain the test source tree")
		}
		return root
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate source file")
	}
	return filepath.Dir(file)
}

// jsonKeyPaths recursively collects JSON object field paths, used to compare the example file with runtime structures.
func jsonKeyPaths(value any, prefix string) []string {
	var out []string
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			out = append(out, path)
			out = append(out, jsonKeyPaths(item, path)...)
		}
	case []any:
		for _, item := range v {
			out = append(out, jsonKeyPaths(item, prefix)...)
		}
	}
	return out
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// The example covers default runtime fields and the global account rule; the mutually exclusive model allowlist is listed separately as an alternative pattern.
func TestConfigExampleCoversEveryRuntimeField(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	exampleSchema := runtimeFileConfig{}
	exampleSchema.Models.Accounts = map[string]modelAccountFileConfig{"all": {}}
	identityEnabled := true
	exampleSchema.Identity = runtimeIdentityConfig{Enabled: &identityEnabled, Target: "WorkBuddy",
		Replacements: defaultIdentityReplacements("WorkBuddy")}
	schema, err := json.Marshal(exampleSchema)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, path := range jsonKeyPaths(decodeObject(t, schema), "") {
		want[path] = true
	}
	got := map[string]bool{}
	for _, path := range jsonKeyPaths(decodeObject(t, raw), "") {
		got[path] = true
	}
	var missing []string
	for path := range want {
		if !got[path] {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("config.example.json is missing config items: %v", missing)
	}
	var extra []string
	for path := range got {
		if !want[path] {
			extra = append(extra, path)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Fatalf("config.example.json contains undefined config items: %v", extra)
	}
}

// Fresh deployment: using the example file directly must start normally and fall to the default ports.
func TestConfigExampleStartsFreshDeployment(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeConfigFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.PortExplicit = false
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatalf("example config failed to load: %v", err)
	}
	if cfg.Port != defaultAPIPort || cfg.WebPort != defaultWebPort {
		t.Fatalf("example config ports=%d/%d, expected %d/%d", cfg.Port, cfg.WebPort, defaultAPIPort, defaultWebPort)
	}
	if currentAdminKey() != "" || currentAPIKey() != "" {
		t.Fatal("example config must not preset any Keys")
	}
}

func TestConfigExampleIncludesIdentityCompatibilityDefaults(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.DeprecatedPromptCompatibility) != 0 {
		t.Fatal("example must not contain the removed compatibility field")
	}
	for old, want := range map[string]string{
		"workspace root":       "workspace base",
		"are paths the user":   "are paths that the user",
		"Grok Build":           "WorkBuddy",
		"Grok":                 "WorkBuddy",
		"Grok released by xAI": "WorkBuddy",
	} {
		if c.Identity.Replacements[old] != want {
			t.Fatalf("config.example identity replacement %q=%q want %q",
				old, c.Identity.Replacements[old], want)
		}
	}
}

// Empty/omitted values: ports 0 or omitted, empty Keys, and false toggles must all start with defaults.
func TestConfigEmptyValuesFallBackToDefaults(t *testing.T) {
	for name, content := range map[string]string{
		"gateway all-empty":     `{"gateway":{"apiPort":0,"webPort":0,"adminKey":"","apiKeyEnabled":false,"apiKey":""}}`,
		"gateway empty object":  `{"gateway":{}}`,
		"gateway fully omitted": `{}`,
		"only ports zero":       `{"gateway":{"apiPort":0,"webPort":0}}`,
	} {
		t.Run(name, func(t *testing.T) {
			isolatedGatewayConfig(t)
			if err := os.WriteFile(runtimeConfigFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.PortExplicit = false
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatalf("empty-value config must start: %v", err)
			}
			if cfg.Port != defaultAPIPort || cfg.WebPort != defaultWebPort {
				t.Fatalf("ports=%d/%d, expected defaults %d/%d", cfg.Port, cfg.WebPort, defaultAPIPort, defaultWebPort)
			}
			if currentAPIKey() != "" || currentAdminKey() != "" {
				t.Fatal("empty-value config must not enable any authentication")
			}
			// With an empty-value config the service must really start (including -webui dual ports).
			cfg.WebUI = true
			cfg.Port, cfg.WebPort = freeTCPPort(t), freeTCPPort(t)
			_, listeners, err := prepareGatewayServers()
			if err != nil {
				t.Fatalf("empty-value config failed to listen: %v", err)
			}
			for _, listener := range listeners {
				_ = listener.Close()
			}
		})
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// Empty Key strings must be normalized to unset values and must not be treated as valid credentials.
func TestConfigBlankKeysAreTreatedAsUnset(t *testing.T) {
	isolatedGatewayConfig(t)
	content := `{"gateway":{"apiPort":8317,"webPort":8316,"adminKey":"","apiKeyEnabled":false,"apiKey":""}}`
	if err := os.WriteFile(runtimeConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(currentAdminKey()) != "" || strings.TrimSpace(currentAPIKey()) != "" {
		t.Fatal("empty Key must not be treated as configured")
	}
	if !reflect.DeepEqual(gatewaySettingsView()["apiKeyEnabled"], false) {
		t.Fatal("apiKeyEnabled=false must stay disabled")
	}
}
