package web

import (
	"testing"
)

func fakeEnv(initial map[string]string) (lookup func(string) (string, bool), set func(string, string) error, dump func() map[string]string) {
	m := map[string]string{}
	for k, v := range initial {
		m[k] = v
	}
	lookup = func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	set = func(k, v string) error { m[k] = v; return nil }
	dump = func() map[string]string { return m }
	return
}

// The console value must win over a pinned environment variable, otherwise
// changing the listen port from the web UI silently does nothing (the original
// bug: M365_LISTEN was set in the systemd EnvironmentFile, so the saved value
// was never applied).
func TestApplyStartupSettingsConsoleWinsOverEnv(t *testing.T) {
	v := defaultRuntimeSettings()
	v.ListenAddress = "127.0.0.1:29422"
	v.ConfigPath = "/tmp/from-console.json"

	lookup, set, dump := fakeEnv(map[string]string{
		"M365_LISTEN": "0.0.0.0:4141", // pinned, as in the deployment
		"M365_CONFIG": "/tmp/from-env.json",
	})
	applyStartupSettings(v, false, lookup, set)

	if got := dump()["M365_LISTEN"]; got != "127.0.0.1:29422" {
		t.Fatalf("console listenAddress did not win: M365_LISTEN=%q", got)
	}
	if got := dump()["M365_CONFIG"]; got != "/tmp/from-console.json" {
		t.Fatalf("console configPath did not win: M365_CONFIG=%q", got)
	}
}

// An empty saved value must not clobber an environment-provided default.
func TestApplyStartupSettingsEmptySavedKeepsEnv(t *testing.T) {
	v := defaultRuntimeSettings()
	v.Authority = "" // nothing saved
	lookup, set, dump := fakeEnv(map[string]string{
		"M365_AUTHORITY": "https://login.microsoftonline.com/common",
	})
	applyStartupSettings(v, false, lookup, set)
	if got := dump()["M365_AUTHORITY"]; got != "https://login.microsoftonline.com/common" {
		t.Fatalf("empty saved value clobbered env: %q", got)
	}
}

// M365_SETTINGS_ENV_WINS=true restores the legacy env-overrides-console order.
func TestApplyStartupSettingsEnvWinsOptOut(t *testing.T) {
	v := defaultRuntimeSettings()
	v.ListenAddress = "127.0.0.1:29422"
	lookup, set, dump := fakeEnv(map[string]string{"M365_LISTEN": "0.0.0.0:4141"})
	applyStartupSettings(v, true, lookup, set)
	if got := dump()["M365_LISTEN"]; got != "0.0.0.0:4141" {
		t.Fatalf("env-wins opt-out not honored: %q", got)
	}
}

func TestRestartStrategyNotPanic(t *testing.T) {
	if s := RestartStrategy(); s == "" {
		t.Fatal("RestartStrategy returned empty")
	}
}
