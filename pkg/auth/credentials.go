package auth

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

// Credentials holds JoyCode authentication data.
type Credentials struct {
	PtKey       string
	UserID      string
	Tenant      string
	LoginType   string
	OrgFullName string
	RealName    string
	// Source describes which client the credentials were loaded from,
	// e.g. "ide", "plugin:code", "container" or "env".
	Source string
	// Models is the model catalog synced by the JoyCode client (chatApiModel
	// ids). Only the VS Code extension state carries it; it feeds the model
	// list shown by the dashboard.
	Models []string
}

// stateData mirrors the JoyCode IDE (Electron) login payload stored under
// key "JoyCoder.IDE" in the IDE's state.vscdb.
type stateData struct {
	JoyCoderUser struct {
		PtKey       string `json:"ptKey"`
		UserID      string `json:"userId"`
		Tenant      string `json:"tenant"`
		LoginType   string `json:"loginType"`
		OrgFullName string `json:"orgFullName"`
	} `json:"joyCoderUser"`
}

// pluginStateData mirrors the payload the JoyCode VS Code extension
// (joycoder.joycoder-fe) stores under its globalState key in the editor's
// shared state.vscdb. Login material lives in "jdhLoginInfo"; the tenant's
// model catalog is synced into "remoteModelConfigs".
type pluginStateData struct {
	JdhLoginInfo struct {
		PtKey       string `json:"ptKey"`
		UserID      string `json:"userId"`
		Tenant      string `json:"tenant"`
		LoginType   string `json:"loginType"`
		OrgFullName string `json:"orgFullName"`
		RealName    string `json:"realName"`
	} `json:"jdhLoginInfo"`
	RemoteModelConfigs []struct {
		ChatAPIModel string `json:"chatApiModel"`
		IsHidden     bool   `json:"isHidden"`
		Hidden       bool   `json:"hidden"`
		Ext          any    `json:"ext"`
	} `json:"remoteModelConfigs"`
}

const (
	stateDBEnv       = "JOYCODE_STATE_DB"
	containerStateDB = "/root/.joycode-ide/state.vscdb"

	ideStateKey    = "JoyCoder.IDE"
	pluginStateKey = "JoyCoder.joycoder-fe"
)

// editorAppDirs lists VS Code-family app dirs that can host the JoyCode extension.
// There is no JoyCode IDE build for linux/amd64, so the extension is the
// primary credential source there.
var editorAppDirs = []string{"Code", "Code - Insiders", "Cursor", "VSCodium", "Windsurf"}

// LoadFromSystem reads ptKey from the local JoyCode IDE state database or,
// on machines without the IDE (linux/amd64), from a VS Code-family editor
// hosting the JoyCode extension.
func LoadFromSystem() (*Credentials, error) {
	// Explicit override wins. The DB may be in either IDE or plugin format —
	// loadFromStateDB auto-detects which key is present.
	if dbPath := os.Getenv(stateDBEnv); dbPath != "" {
		creds, err := loadFromStateDB(dbPath)
		if err != nil {
			return nil, err
		}
		creds.Source = "env"
		return creds, nil
	}
	if _, err := os.Stat(containerStateDB); err == nil {
		creds, err := loadFromStateDB(containerStateDB)
		if err != nil {
			return nil, err
		}
		creds.Source = "container"
		return creds, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}

	// 1) JoyCode IDE (Electron) state — only shipped for macOS / Windows /
	//    arm64 Linux, but try it everywhere so existing installs keep working.
	var probeErrors []string
	ideDB := filepath.Join(appConfigDir(home),
		"JoyCode", "User", "globalStorage", "state.vscdb")
	if _, err := os.Stat(ideDB); err == nil {
		creds, err := loadFromStateDB(ideDB)
		if err == nil {
			creds.Source = "ide"
			return creds, nil
		}
		// A stale/logged-out IDE database must not mask a valid editor plugin.
		probeErrors = append(probeErrors, fmt.Sprintf("%s: %v", ideDB, err))
	}

	// 2) JoyCode extension inside a VS Code-family editor — the linux/amd64
	//    story. Scan known editors for a globalStorage DB that contains the
	//    extension's login payload.
	for _, candidate := range pluginStateDBCandidates(home) {
		if _, err := os.Stat(candidate.Path); err != nil {
			continue
		}
		creds, err := loadPluginStateDB(candidate.Path)
		if err != nil {
			probeErrors = append(probeErrors, fmt.Sprintf("%s: %v", candidate.Path, err))
			continue
		}
		creds.Source = "plugin:" + candidate.Source
		return creds, nil
	}

	msg := fmt.Sprintf("no JoyCode credentials found: no IDE state at %s and no logged-in JoyCode extension in any of [%s]",
		ideDB, strings.Join(editorAppDirs, ", "))
	if len(probeErrors) > 0 {
		msg += "\n  plugin probes failed: " + strings.Join(probeErrors, "; ")
	}
	msg += "\n  Please log in to JoyCode IDE or the JoyCode editor extension first"
	return nil, fmt.Errorf("%s", msg)
}

type stateDBCandidate struct {
	Path   string
	Source string
}

func pluginStateDBCandidates(home string) []stateDBCandidate {
	candidates := make([]stateDBCandidate, 0, len(editorAppDirs)+3)
	for _, editor := range editorAppDirs {
		candidates = append(candidates, stateDBCandidate{
			Path:   filepath.Join(appConfigDir(home), editor, "User", "globalStorage", "state.vscdb"),
			Source: editorSlug(editor),
		})
	}
	// Remote/headless Linux installations keep global state outside XDG config.
	for _, server := range []string{".vscode-server", ".vscode-server-insiders", ".cursor-server"} {
		candidates = append(candidates, stateDBCandidate{
			Path:   filepath.Join(home, server, "data", "User", "globalStorage", "state.vscdb"),
			Source: strings.TrimPrefix(server, "."),
		})
	}
	return candidates
}

// appConfigDir returns the per-user application config directory for the
// current OS (AppData on Windows, Application Support on macOS, XDG config
// home elsewhere).
func appConfigDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return appData
		}
		return filepath.Join(home, "AppData", "Roaming")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return xdg
		}
		return filepath.Join(home, ".config")
	}
}

// editorSlug normalizes an editor dir name into a source identifier,
// e.g. "Code - Insiders" → "code-insiders".
func editorSlug(editor string) string {
	return strings.ToLower(strings.ReplaceAll(editor, " - ", "-"))
}

// loadFromStateDB opens a state.vscdb and reads whichever JoyCode login
// payload it contains: the IDE key first, then the extension key.
func loadFromStateDB(dbPath string) (*Credentials, error) {
	db, err := openStateDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if value, err := queryStateValue(db, ideStateKey); err == nil {
		return parseIDEState(value)
	}
	if value, err := queryStateValue(db, pluginStateKey); err == nil {
		return parsePluginState(value)
	}
	return nil, fmt.Errorf("login info not found in database\n  Please log in to JoyCode IDE or the JoyCode editor extension first")
}

// loadPluginStateDB reads credentials from an editor globalStorage DB that
// only carries the extension payload.
func loadPluginStateDB(dbPath string) (*Credentials, error) {
	db, err := openStateDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	value, err := queryStateValue(db, pluginStateKey)
	if err != nil {
		return nil, fmt.Errorf("JoyCode extension login info not found in database\n  Please log in to the JoyCode extension first")
	}
	return parsePluginState(value)
}

func openStateDB(dbPath string) (*sql.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("JoyCode state database not found at %s: %w", dbPath, err)
	}

	// modernc 不支持 mattn 的 ?mode=ro query；用 SQLite URI 以真正的只读共享锁
	// 打开 JoyCode IDE 的库，避免 IDE 运行时拿不到锁。
	db, err := sql.Open("sqlite", sqliteReadOnlyURI(dbPath))
	if err != nil {
		return nil, fmt.Errorf("cannot open JoyCode database: %w", err)
	}
	return db, nil
}

func queryStateValue(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM ItemTable WHERE key=?", key).Scan(&value)
	return value, err
}

func parseIDEState(value string) (*Credentials, error) {
	var data stateData
	if err := json.Unmarshal([]byte(value), &data); err != nil {
		return nil, fmt.Errorf("cannot parse login data from database: %w", err)
	}
	if data.JoyCoderUser.PtKey == "" {
		return nil, fmt.Errorf("ptKey is empty in stored credentials\n  Please re-login to JoyCode IDE")
	}
	if data.JoyCoderUser.UserID == "" {
		return nil, fmt.Errorf("userId is empty in stored credentials\n  Please re-login to JoyCode IDE")
	}
	return &Credentials{
		PtKey:       data.JoyCoderUser.PtKey,
		UserID:      data.JoyCoderUser.UserID,
		Tenant:      data.JoyCoderUser.Tenant,
		LoginType:   data.JoyCoderUser.LoginType,
		OrgFullName: data.JoyCoderUser.OrgFullName,
	}, nil
}

func parsePluginState(value string) (*Credentials, error) {
	var data pluginStateData
	if err := json.Unmarshal([]byte(value), &data); err != nil {
		return nil, fmt.Errorf("cannot parse extension login data from database: %w", err)
	}
	login := data.JdhLoginInfo
	if login.PtKey == "" {
		return nil, fmt.Errorf("ptKey is empty in extension credentials\n  Please re-login to the JoyCode extension")
	}
	if login.UserID == "" {
		return nil, fmt.Errorf("userId is empty in extension credentials\n  Please re-login to the JoyCode extension")
	}

	// Harvest the visible tenant model catalog (chatApiModel ids) for the
	// dashboard's model list.
	models := make([]string, 0, len(data.RemoteModelConfigs))
	seen := make(map[string]bool, len(data.RemoteModelConfigs))
	for _, m := range data.RemoteModelConfigs {
		name := strings.TrimSpace(m.ChatAPIModel)
		if name == "" || m.IsHidden || m.Hidden || seen[name] {
			continue
		}
		seen[name] = true
		models = append(models, name)
	}

	return &Credentials{
		PtKey:       login.PtKey,
		UserID:      login.UserID,
		Tenant:      login.Tenant,
		LoginType:   login.LoginType,
		OrgFullName: login.OrgFullName,
		RealName:    login.RealName,
		Models:      models,
	}, nil
}

// sqliteReadOnlyURI builds a cross-platform SQLite URI with read-only mode.
// On Windows, absolute paths like C:\Users\... must become /C:/Users/... in URI form.
func sqliteReadOnlyURI(path string) string {
	p := filepath.ToSlash(path)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}
	return "file:" + p + "?mode=ro"
}
