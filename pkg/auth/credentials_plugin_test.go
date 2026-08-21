package auth

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite"
)

// buildPluginPayload returns a minimal JoyCoder.joycoder-fe globalState blob.
func buildPluginPayload() string {
	return `{
	  "jdhLoginInfo": {
	    "userId": "jd_test123",
	    "erp": "test.erp",
	    "loginType": "ERP",
	    "ptKey": "plugin-pt-key-9",
	    "tenant": "JD",
	    "realName": "测试用户",
	    "orgName": "京东集团",
	    "orgFullName": "京东集团-xx实验室",
	    "masterBaseUrl": "http://joycode-api-saas.jd.com",
	    "colorBaseUrl": "https://api-ai.jd.com",
	    "slaveBaseUrl": "http://joycode-api-saas.jd.com"
	  },
	  "remoteModelConfigs": [
	    {"chatApiModel": "JoyAI-Code-1.5", "isHidden": false, "hidden": false},
	    {"chatApiModel": "GPT-5.6 Sol", "isHidden": false, "hidden": false, "ext": "{\"adapterType\":\"openai-response\"}"},
	    {"chatApiModel": "Claude-Opus-4.7-hq", "isHidden": false, "hidden": false, "ext": {"adapterType":"anthropic"}},
	    {"chatApiModel": "Claude-Sonnet-4.6-hq", "isHidden": false, "hidden": false},
	    {"chatApiModel": "Internal-Hidden", "isHidden": true, "hidden": false},
	    {"chatApiModel": "", "isHidden": false, "hidden": false}
	  ]
	}`
}

func mustWriteStateDB(t *testing.T, dbPath string, rows map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for k, v := range rows {
		if _, err := db.Exec("INSERT INTO ItemTable (key, value) VALUES (?, ?)", k, v); err != nil {
			t.Fatalf("insert %s: %v", k, err)
		}
	}
}

func TestParsePluginState_FullFields(t *testing.T) {
	creds, err := parsePluginState(buildPluginPayload())
	if err != nil {
		t.Fatalf("parsePluginState error: %v", err)
	}
	if creds.PtKey != "plugin-pt-key-9" {
		t.Errorf("PtKey = %q", creds.PtKey)
	}
	if creds.UserID != "jd_test123" {
		t.Errorf("UserID = %q", creds.UserID)
	}
	if creds.LoginType != "ERP" {
		t.Errorf("LoginType = %q, want ERP (the plugin logs in as ERP, not PIN_JD_CLOUD)", creds.LoginType)
	}
	if creds.Tenant != "JD" {
		t.Errorf("Tenant = %q", creds.Tenant)
	}
	if creds.MasterBaseURL != "http://joycode-api-saas.jd.com" {
		t.Errorf("MasterBaseURL = %q", creds.MasterBaseURL)
	}
	if creds.ColorBaseURL != "https://api-ai.jd.com" {
		t.Errorf("ColorBaseURL = %q", creds.ColorBaseURL)
	}
	if creds.RealName != "测试用户" {
		t.Errorf("RealName = %q", creds.RealName)
	}
	wantModels := []string{"JoyAI-Code-1.5", "GPT-5.6 Sol", "Claude-Opus-4.7-hq", "Claude-Sonnet-4.6-hq"}
	if len(creds.Models) != len(wantModels) {
		t.Fatalf("Models = %v, want %v", creds.Models, wantModels)
	}
	for i, m := range wantModels {
		if creds.Models[i] != m {
			t.Errorf("Models[%d] = %q, want %q", i, creds.Models[i], m)
		}
	}
	if got := creds.ModelAdapters["GPT-5.6 Sol"]; got != "openai-response" {
		t.Errorf("GPT adapter = %q", got)
	}
	if got := creds.ModelAdapters["Claude-Opus-4.7-hq"]; got != "anthropic" {
		t.Errorf("Claude adapter = %q", got)
	}
}

func TestParsePluginState_EmptyPtKey(t *testing.T) {
	_, err := parsePluginState(`{"jdhLoginInfo":{"userId":"u1","ptKey":""}}`)
	if err == nil {
		t.Fatal("expected error for empty ptKey")
	}
}

func TestParsePluginState_MissingLoginInfo(t *testing.T) {
	_, err := parsePluginState(`{"someOtherKey":{}}`)
	if err == nil {
		t.Fatal("expected error when jdhLoginInfo is absent")
	}
}

func TestLoadPluginStateDB_RoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	mustWriteStateDB(t, dbPath, map[string]string{pluginStateKey: buildPluginPayload()})

	creds, err := loadPluginStateDB(dbPath)
	if err != nil {
		t.Fatalf("loadPluginStateDB: %v", err)
	}
	if creds.PtKey != "plugin-pt-key-9" || creds.UserID != "jd_test123" {
		t.Errorf("got ptKey=%q userId=%q", creds.PtKey, creds.UserID)
	}
}

func TestLoadPluginStateDB_NoPluginKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	mustWriteStateDB(t, dbPath, map[string]string{"some.other.extension": "{}"})

	if _, err := loadPluginStateDB(dbPath); err == nil {
		t.Fatal("expected error when plugin key is missing")
	}
}

// On linux test runners this exercises the real LoadFromSystem plugin path:
// a JoyCode extension login living in VS Code's globalStorage.
func TestLoadFromSystem_ViaEditorPlugin(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("plugin-path layout is asserted on linux only")
	}
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, ".config", "Code", "User", "globalStorage", "state.vscdb")
	mustWriteStateDB(t, dbPath, map[string]string{pluginStateKey: buildPluginPayload()})

	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(stateDBEnv, "")

	creds, err := LoadFromSystem()
	if err != nil {
		t.Fatalf("LoadFromSystem: %v", err)
	}
	if creds.Source != "plugin:code" {
		t.Errorf("Source = %q, want plugin:code", creds.Source)
	}
	if creds.PtKey != "plugin-pt-key-9" || creds.LoginType != "ERP" {
		t.Errorf("got ptKey=%q loginType=%q", creds.PtKey, creds.LoginType)
	}
	if len(creds.Models) == 0 {
		t.Error("expected model catalog to be harvested from plugin state")
	}
}

// JOYCODE_STATE_DB must accept the plugin payload format too, not only the
// IDE one (the env var is used for Docker-mounted state files).
func TestLoadFromSystem_EnvOverridePluginFormat(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	mustWriteStateDB(t, dbPath, map[string]string{pluginStateKey: buildPluginPayload()})
	t.Setenv(stateDBEnv, dbPath)

	creds, err := LoadFromSystem()
	if err != nil {
		t.Fatalf("LoadFromSystem: %v", err)
	}
	if creds.Source != "env" {
		t.Errorf("Source = %q, want env", creds.Source)
	}
	if creds.UserID != "jd_test123" {
		t.Errorf("UserID = %q", creds.UserID)
	}
}

func TestLoadFromSystem_ViaVSCodeServerPlugin(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("server-path layout is asserted on linux only")
	}
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, ".vscode-server", "data", "User", "globalStorage", "state.vscdb")
	mustWriteStateDB(t, dbPath, map[string]string{pluginStateKey: buildPluginPayload()})
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(stateDBEnv, "")

	creds, err := LoadFromSystem()
	if err != nil {
		t.Fatalf("LoadFromSystem: %v", err)
	}
	if creds.Source != "plugin:vscode-server" {
		t.Errorf("Source = %q", creds.Source)
	}
}

func TestEditorSlug(t *testing.T) {
	if got := editorSlug("Code - Insiders"); got != "code-insiders" {
		t.Errorf("editorSlug = %q", got)
	}
	if got := editorSlug("Code"); got != "code" {
		t.Errorf("editorSlug = %q", got)
	}
}
