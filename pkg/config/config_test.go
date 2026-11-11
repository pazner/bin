package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name               string
		in                 string
		wantTree, wantPath string
	}{
		{"bin dir", `{"default_path": "/usr/local/bin"}`, "/usr/local", ""},
		{"trailing slash", `{"default_path": "/usr/local/bin/"}`, "/usr/local", ""},
		{"env var", `{"default_path": "$HOME/.local/bin"}`, "$HOME/.local", ""},
		{"not a bin dir", `{"default_path": "/opt/tools"}`, "", "/opt/tools"},
		{"tree already set", `{"default_tree": "/x", "default_path": "/opt/tools"}`, "/x", "/opt/tools"},
	}
	for _, c := range cases {
		var conf config
		if err := json.Unmarshal([]byte(c.in), &conf); err != nil {
			t.Fatal(err)
		}
		if !migrate(&conf) {
			t.Errorf("%s: config not migrated", c.name)
		}
		if conf.DefaultTree != c.wantTree || conf.DefaultPath != c.wantPath || conf.Version != currentVersion {
			t.Errorf("%s: got tree %q, path %q, version %d; want %q, %q, %d", c.name, conf.DefaultTree, conf.DefaultPath, conf.Version, c.wantTree, c.wantPath, currentVersion)
		}
	}

	conf := config{Bins: map[string]*Binary{
		"a": {Path: "a"},
		"b": {Path: "b", Mode: ModeFull},
	}}
	migrate(&conf)
	if conf.Bins["a"].Mode != ModeBinary || conf.Bins["b"].Mode != ModeFull {
		t.Errorf("unexpected modes after migration: %q, %q", conf.Bins["a"].Mode, conf.Bins["b"].Mode)
	}

	// Current configs are left alone, even with entries without a mode
	// (e.g. edited by hand).
	conf = config{Version: currentVersion, DefaultPath: "/usr/local/bin", Bins: map[string]*Binary{"a": {}}}
	if migrate(&conf) || conf.DefaultPath != "/usr/local/bin" || conf.Bins["a"].Mode != "" {
		t.Errorf("current config was changed: %+v", conf)
	}
}

// TestCheckAndLoadMigrates checks that an old config file is upgraded and
// written back on load.
func TestCheckAndLoadMigrates(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	p := filepath.Join(cfgHome, "bin", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"default_path": "/usr/local/bin", "bins": {"/usr/local/bin/x": {"path": "/usr/local/bin/x", "version": "1"}}}`
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckAndLoad(); err != nil {
		t.Fatal(err)
	}
	if got := Get().BinDir(); got != "/usr/local/bin" {
		t.Errorf("BinDir() = %s, want /usr/local/bin", got)
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]any
	if err := json.Unmarshal(b, &written); err != nil {
		t.Fatal(err)
	}
	if written["version"] != float64(currentVersion) || written["default_tree"] != "/usr/local" || written["default_path"] != nil {
		t.Errorf("unexpected config written:\n%s", b)
	}
	if mode := written["bins"].(map[string]any)["/usr/local/bin/x"].(map[string]any)["mode"]; mode != ModeBinary {
		t.Errorf("entry mode written as %v", mode)
	}
}

func TestExpandPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TREE", "/srv")
	for in, want := range map[string]string{
		"~":          home,
		"~/ws/local": filepath.Join(home, "ws", "local"),
		"$TREE/x":    "/srv/x",
		"/abs":       "/abs",
		"a~b":        "a~b",
	} {
		if got := ExpandPath(in); got != want {
			t.Errorf("ExpandPath(%q) = %q, want %q", in, got, want)
		}
	}
}
