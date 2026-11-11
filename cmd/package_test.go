//go:build !windows

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/marcosnils/bin/pkg/assets"
	"github.com/marcosnils/bin/pkg/config"
	"github.com/marcosnils/bin/pkg/providers"
)

func TestSupportFiles(t *testing.T) {
	entries := []assets.ArchiveEntry{
		{Name: "tool-v1/tool", Mode: 0o755, Data: []byte("bin")},
		{Name: "tool-v1/man/tool.1", Mode: 0o644, Data: []byte(".TH TOOL 1")},
		{Name: "tool-v1/man/tool.conf.5.gz", Mode: 0o644, Data: []byte{0x1f, 0x8b, 0}},
		// Not man pages: a versioned library, an executable and plain text.
		{Name: "tool-v1/lib/libtool.so.1", Mode: 0o644, Data: []byte(".ELF")},
		{Name: "tool-v1/tool.1", Mode: 0o755, Data: []byte(".TH")},
		{Name: "tool-v1/NOTES.1", Mode: 0o644, Data: []byte("notes")},
		{Name: "tool-v1/completions/tool.bash", Mode: 0o644, Data: []byte("complete")},
		{Name: "tool-v1/completions/_tool", Mode: 0o644, Data: []byte("#compdef tool")},
		{Name: "tool-v1/contrib/completion/fish/tool", Mode: 0o644, Data: []byte("complete -c tool")},
		// A script that isn't a completion.
		{Name: "tool-v1/scripts/setup.bash", Mode: 0o644, Data: []byte("echo")},
	}

	got := supportFiles(entries, "tool")
	want := map[string]string{
		filepath.Join("man", "man1", "tool.1"):                     "tool-v1/man/tool.1",
		filepath.Join("man", "man5", "tool.conf.5.gz"):             "tool-v1/man/tool.conf.5.gz",
		filepath.Join("bash-completion", "completions", "tool"):    "tool-v1/completions/tool.bash",
		filepath.Join("zsh", "site-functions", "_tool"):            "tool-v1/completions/_tool",
		filepath.Join("fish", "vendor_completions.d", "tool.fish"): "tool-v1/contrib/completion/fish/tool",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("supportFiles:\n got %v\nwant %v", got, want)
	}

	// Several completions for one shell, none named after the command, are
	// ambiguous and skipped.
	got = supportFiles([]assets.ArchiveEntry{
		{Name: "completions/a.bash", Data: []byte("x")},
		{Name: "completions/b.bash", Data: []byte("x")},
	}, "tool")
	if len(got) != 0 {
		t.Errorf("ambiguous completions: got %v, want none", got)
	}
}

func TestExtractArchiveUnsafePaths(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/evil", "a/../../evil"} {
		dir := t.TempDir()
		err := extractArchive([]assets.ArchiveEntry{{Name: name, Data: []byte("x")}}, dir)
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	dir := t.TempDir()
	err := extractArchive([]assets.ArchiveEntry{
		{Name: "pkg/lib/libtool.so.1.0", Mode: 0o644, Data: []byte("lib")},
		{Name: "pkg/lib/libtool.so.1", Linkname: "libtool.so.1.0"},
		{Name: "pkg/abs", Linkname: "/etc/passwd"},
		{Name: "pkg/up", Linkname: "../../outside"},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pkg/lib/libtool.so.1")); err != nil || string(b) != "lib" {
		t.Errorf("relative symlink not extracted: %q, %v", b, err)
	}
	for _, n := range []string{"pkg/abs", "pkg/up"} {
		if _, err := os.Lstat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s: symlink escaping the package was extracted", n)
		}
	}
}

// setupTestConfig points bin's config and default tree at temp dirs and
// returns the tree's bin and share dirs.
func setupTestConfig(t *testing.T) (binDir, dataDir string) {
	t.Helper()
	cfgHome := t.TempDir()
	tree := t.TempDir()
	binDir = filepath.Join(tree, "bin")
	dataDir = filepath.Join(tree, "share")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, d := range []string{filepath.Join(cfgHome, "bin"), binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgJSON := `{"version": 1, "default_tree": "` + tree + `", "bins": {}}`
	if err := os.WriteFile(filepath.Join(cfgHome, "bin", "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.CheckAndLoad(); err != nil {
		t.Fatal(err)
	}
	return binDir, dataDir
}

func packageFile(version string, entries map[string]string) *providers.File {
	f := &providers.File{Name: "tool", Version: version, PackagePath: "tool-" + version + "/tool"}
	for n, c := range entries {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(n, "/tool") {
			mode = 0o755
			f.Data = bytes.NewReader([]byte(c))
		}
		f.Archive = append(f.Archive, assets.ArchiveEntry{Name: n, Mode: mode, Data: []byte(c)})
	}
	return f
}

func TestInstallPackageLifecycle(t *testing.T) {
	binDir, dataDir := setupTestConfig(t)
	dst := filepath.Join(binDir, "tool")

	// A completion the user put there themselves must be left alone.
	userFish := filepath.Join(dataDir, "fish", "vendor_completions.d", "tool.fish")
	if err := os.MkdirAll(filepath.Dir(userFish), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userFish, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	v1 := packageFile("v1", map[string]string{
		"tool-v1/tool":                  "tool v1",
		"tool-v1/share/tool/data.txt":   "data v1",
		"tool-v1/man/tool.1":            ".TH TOOL 1 v1",
		"tool-v1/completions/tool.bash": "bash v1",
		"tool-v1/completions/tool.fish": "fish v1",
		"tool-v1/completions/_tool":     "zsh v1",
	})
	res, err := installBinary(v1, dst, false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := res.apply(&config.Binary{Path: dst})

	pkgDir := filepath.Join(dataDir, "bin", "packages", "tool")
	if b.PackageDir != pkgDir {
		t.Errorf("got package dir %s, want %s", b.PackageDir, pkgDir)
	}
	if target, _ := os.Readlink(dst); target != filepath.Join(pkgDir, "tool-v1", "tool") {
		t.Errorf("binary links to %q", target)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("binary not installed executable: %v, %v", fi, err)
	}
	assertFile(t, filepath.Join(pkgDir, "tool-v1", "share", "tool", "data.txt"), "data v1")
	assertFile(t, filepath.Join(dataDir, "man", "man1", "tool.1"), ".TH TOOL 1 v1")
	assertFile(t, filepath.Join(dataDir, "bash-completion", "completions", "tool"), "bash v1")
	assertFile(t, filepath.Join(dataDir, "zsh", "site-functions", "_tool"), "zsh v1")
	assertFile(t, userFish, "mine")
	if len(b.Links) != 3 {
		t.Errorf("got links %v, want man page, bash and zsh completions", b.Links)
	}

	// Without --force an existing binary isn't replaced.
	if _, err := installBinary(packageFile("v1", map[string]string{"tool-v1/tool": "x"}), dst, false, true, b); err == nil {
		t.Error("expected an error installing over an existing binary")
	}

	// Upgrading swaps the package and refreshes the links; the zsh
	// completion is gone in the new version.
	v2 := packageFile("v2", map[string]string{
		"tool-v2/tool":                  "tool v2",
		"tool-v2/man/tool.1":            ".TH TOOL 1 v2",
		"tool-v2/completions/tool.bash": "bash v2",
	})
	res, err = installBinary(v2, dst, true, true, b)
	if err != nil {
		t.Fatal(err)
	}
	b = res.apply(&config.Binary{Path: dst})
	assertFile(t, dst, "tool v2")
	assertFile(t, filepath.Join(dataDir, "man", "man1", "tool.1"), ".TH TOOL 1 v2")
	assertFile(t, filepath.Join(dataDir, "bash-completion", "completions", "tool"), "bash v2")
	if _, err := os.Lstat(filepath.Join(dataDir, "zsh", "site-functions", "_tool")); !os.IsNotExist(err) {
		t.Error("stale zsh completion link was kept")
	}
	if _, err := os.Stat(filepath.Join(pkgDir, "tool-v1")); !os.IsNotExist(err) {
		t.Error("previous package contents were kept")
	}
	if entries, _ := os.ReadDir(filepath.Dir(pkgDir)); len(entries) != 1 {
		t.Errorf("leftover temp dirs in packages root: %v", entries)
	}

	removePackage(b, true)
	for _, p := range append(b.Links, dst, pkgDir) {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s not removed", p)
		}
	}
	assertFile(t, userFish, "mine")
}

// Reinstalling without --full cleans up the previous package install.
func TestInstallBinaryReplacesPackage(t *testing.T) {
	binDir, _ := setupTestConfig(t)
	dst := filepath.Join(binDir, "tool")

	res, err := installBinary(packageFile("v1", map[string]string{
		"tool-v1/tool":       "tool v1",
		"tool-v1/man/tool.1": ".TH TOOL 1",
	}), dst, false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := res.apply(&config.Binary{Path: dst})

	f := &providers.File{Data: strings.NewReader("plain"), Name: "tool", Version: "v2"}
	res, err = installBinary(f, dst, true, false, b)
	if err != nil {
		t.Fatal(err)
	}
	if res.packageDir != "" || len(res.links) != 0 {
		t.Errorf("unexpected package install result %+v", res)
	}
	if fi, err := os.Lstat(dst); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("binary should be a regular file: %v, %v", fi, err)
	}
	assertFile(t, dst, "plain")
	for _, p := range append(b.Links, b.PackageDir) {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s not removed", p)
		}
	}
}

func assertFile(t *testing.T, p, want string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Errorf("reading %s: %v", p, err)
	} else if string(b) != want {
		t.Errorf("%s: got %q, want %q", p, b, want)
	}
}

func TestShareDir(t *testing.T) {
	home := t.TempDir()
	data := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", data)

	cases := map[string]string{
		filepath.Join(home, ".local", "bin"):      filepath.Join(home, ".local", "share"),
		filepath.Join(home, "ws", "local", "bin"): filepath.Join(home, "ws", "local", "share"),
		"~/ws/local/bin":                          filepath.Join(home, "ws", "local", "share"),
		"/usr/local/bin":                          "/usr/local/share",
		filepath.Join(home, "bin"):                data, // never ~/share
		"/opt/tools":                              data, // not a bin dir
	}
	for binDir, want := range cases {
		if got, err := shareDir(binDir); err != nil || got != want {
			t.Errorf("shareDir(%s) = %s, %v; want %s", binDir, got, err, want)
		}
	}
}

func TestInstallResultApplyMode(t *testing.T) {
	full := &installResult{packageDir: "/x/bin/packages/tool"}
	plain := &installResult{}
	if got := full.apply(&config.Binary{}).Mode; got != config.ModeFull {
		t.Errorf("package install recorded as %q", got)
	}
	if got := plain.apply(&config.Binary{Mode: config.ModeFull}).Mode; got != config.ModeBinary {
		t.Errorf("binary install recorded as %q", got)
	}
}

func TestOrphanedPackageFiles(t *testing.T) {
	binDir, dataDir := setupTestConfig(t)
	install := func(name string) *config.Binary {
		t.Helper()
		f := packageFile("v1", map[string]string{
			"tool-v1/tool":       name,
			"tool-v1/man/tool.1": ".TH " + name,
		})
		f.Name = name
		dst := filepath.Join(binDir, name)
		res, err := installBinary(f, dst, false, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		// man pages are named after the archive entry, keep them apart
		return res.apply(&config.Binary{Path: dst})
	}
	kept := install("kept")
	// "gone" mimics an entry rewritten by an older bin, which dropped its
	// package fields and replaced the binary link with a plain file.
	gone := install("gone")
	goneDir := gone.PackageDir
	if err := os.Remove(gone.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gone.Path, []byte("plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := &config.Binary{Path: gone.Path}

	// A leftover from an interrupted install and a stray binary link.
	root := filepath.Join(dataDir, "bin", "packages")
	leftover := filepath.Join(root, ".tool.new-123")
	if err := os.Mkdir(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(binDir, "stray")
	if err := os.Symlink(filepath.Join(goneDir, "tool-v1", "tool"), stray); err != nil {
		t.Fatal(err)
	}

	bins := map[string]*config.Binary{kept.Path: kept, legacy.Path: legacy}
	got := orphanedPackageFiles(bins, bins, binDir)
	sort.Strings(got)
	want := []string{leftover, goneDir, stray}
	sort.Strings(want)
	// The man page link of "gone" collided with "kept"'s and was skipped,
	// so only "kept" has one.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("orphans:\n got %v\nwant %v", got, want)
	}
	for _, p := range append(kept.Links, kept.Path, kept.PackageDir, gone.Path) {
		for _, o := range got {
			if o == p {
				t.Errorf("%s reported as orphan", p)
			}
		}
	}
}

func TestConvertToBinary(t *testing.T) {
	binDir, _ := setupTestConfig(t)
	dst := filepath.Join(binDir, "tool")
	res, err := installBinary(packageFile("v1", map[string]string{
		"tool-v1/tool":       "tool v1",
		"tool-v1/man/tool.1": ".TH TOOL 1",
	}), dst, false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := res.apply(&config.Binary{Path: dst, Version: "v1", Pinned: true})
	if err := config.UpsertBinary(b); err != nil {
		t.Fatal(err)
	}

	if err := convertBinary(b, false, true); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(dst); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dry run converted the binary")
	}

	if err := convertBinary(b, false, false); err != nil {
		t.Fatal(err)
	}
	got := config.Get().Bins[dst]
	if got.Mode != config.ModeBinary || got.PackageDir != "" || got.Links != nil || !got.Pinned || got.Hash != b.Hash {
		t.Errorf("unexpected entry after conversion: %+v", got)
	}
	if fi, err := os.Lstat(dst); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("binary should be a regular file: %v, %v", fi, err)
	}
	assertFile(t, dst, "tool v1")
	for _, p := range append(b.Links, b.PackageDir) {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s not removed", p)
		}
	}

	// An entry without a mode only gets its mode recorded.
	legacy := &config.Binary{Path: dst, Version: "v1"}
	if err := convertBinary(legacy, false, false); err != nil {
		t.Fatal(err)
	}
	if m := config.Get().Bins[dst].Mode; m != config.ModeBinary {
		t.Errorf("legacy entry recorded as %q", m)
	}

	// Providers without packages are skipped without downloading.
	docker := &config.Binary{Path: dst, Provider: "docker", Mode: config.ModeBinary}
	if err := convertBinary(docker, true, false); err != nil {
		t.Fatal(err)
	}
}

func TestFullByDefault(t *testing.T) {
	setupTestConfig(t)
	cfg := config.Get()
	for mode, want := range map[string]bool{"": true, config.ModeFull: true, config.ModeBinary: false, "bogus": true} {
		cfg.DefaultMode = mode
		if got := cfg.FullByDefault(); got != want {
			t.Errorf("default_mode %q: FullByDefault() = %v, want %v", mode, got, want)
		}
	}
	// Entries without a recorded mode are binary-only.
	if (&config.Binary{}).IsFull() {
		t.Error("entry without a mode treated as full")
	}
}

func TestPrintConfig(t *testing.T) {
	binDir, dataDir := setupTestConfig(t)
	config.Get().DefaultMode = config.ModeBinary

	var buf bytes.Buffer
	if err := printConfig(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	path, _ := config.Path()
	for _, want := range []string{"Config file: " + path, binDir, dataDir} {
		if !strings.Contains(out, want) {
			t.Errorf("output doesn't contain %q:\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`default_mode\s+binary\s+binary`).MatchString(out) {
		t.Errorf("default_mode not shown:\n%s", out)
	}
}
