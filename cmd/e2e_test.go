//go:build !windows

package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcosnils/bin/pkg/config"
)

// runBin runs bin with the given arguments, like the CLI does.
func runBin(t *testing.T, args ...string) error {
	t.Helper()
	r := newRootCmd("test", func(int) {})
	r.cmd.SetArgs(args)
	return r.cmd.Execute()
}

// writeTarGz writes a release archive with a binary, a man page and a bash
// completion, all with the given content.
func writeTarGz(t *testing.T, path, content string) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, mode := range map[string]int64{
		"tool/tool":                  0o755,
		"tool/man/tool.1":            0o644,
		"tool/completions/tool.bash": 0o644,
	} {
		data := []byte(content)
		if strings.HasSuffix(name, ".1") {
			data = []byte(".TH " + content)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = gw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLocalLifecycle drives install, update, convert, ensure, prune and
// remove through the CLI with a release archive from the local filesystem.
func TestLocalLifecycle(t *testing.T) {
	binDir, dataDir := setupTestConfig(t)
	src := t.TempDir()
	archive := filepath.Join(src, "tool.tar.gz")
	writeTarGz(t, archive, "v1")

	tool := filepath.Join(binDir, "tool")
	manPage := filepath.Join(dataDir, "man", "man1", "tool.1")
	completion := filepath.Join(dataDir, "bash-completion", "completions", "tool")
	entry := func() *config.Binary {
		t.Helper()
		if err := config.CheckAndLoad(); err != nil {
			t.Fatal(err)
		}
		b := config.Get().Bins[tool]
		if b == nil {
			t.Fatalf("%s not in config", tool)
		}
		return b
	}
	isLink := func(p string) bool {
		fi, err := os.Lstat(p)
		return err == nil && fi.Mode()&os.ModeSymlink != 0
	}

	// Install: full by default, URL stored as an absolute file URL.
	if err := runBin(t, "install", archive); err != nil {
		t.Fatal(err)
	}
	b := entry()
	if b.Provider != "local" || b.URL != "file://"+archive || b.Mode != config.ModeFull || !strings.HasPrefix(b.Version, "sha256:") {
		t.Errorf("unexpected entry after install: %+v", b)
	}
	if !isLink(tool) {
		t.Error("binary isn't a link into the package")
	}
	assertFile(t, tool, "v1")
	assertFile(t, manPage, ".TH v1")
	assertFile(t, completion, "v1")
	v1 := b.Version

	// Update picks up the rebuilt archive.
	writeTarGz(t, archive, "v2")
	if err := runBin(t, "update", "-y"); err != nil {
		t.Fatal(err)
	}
	if b = entry(); b.Version == v1 || b.Mode != config.ModeFull {
		t.Errorf("unexpected entry after update: %+v", b)
	}
	assertFile(t, tool, "v2")
	assertFile(t, manPage, ".TH v2")

	// Convert to binary-only and back, which re-reads the local file.
	if err := runBin(t, "convert", "--binary", tool); err != nil {
		t.Fatal(err)
	}
	if b = entry(); b.Mode != config.ModeBinary || isLink(tool) {
		t.Errorf("not converted to a binary install: %+v", b)
	}
	if _, err := os.Lstat(manPage); !os.IsNotExist(err) {
		t.Error("man page link kept after converting to a binary install")
	}
	if err := runBin(t, "convert", "--full", tool); err != nil {
		t.Fatal(err)
	}
	if b = entry(); b.Mode != config.ModeFull || !isLink(tool) {
		t.Errorf("not converted to a full install: %+v", b)
	}
	assertFile(t, manPage, ".TH v2")

	// With the binary and its source gone, ensure warns and skips it.
	moved := archive + ".moved"
	if err := os.Rename(archive, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	if err := runBin(t, "ensure"); err != nil {
		t.Errorf("ensure failed on a missing source: %v", err)
	}
	// update skips it as well.
	if err := runBin(t, "update", "-y"); err != nil {
		t.Errorf("update failed on a missing source: %v", err)
	}

	// Once the source is back, ensure reinstalls it.
	if err := os.Rename(moved, archive); err != nil {
		t.Fatal(err)
	}
	if err := runBin(t, "ensure"); err != nil {
		t.Fatal(err)
	}
	assertFile(t, tool, "v2")

	// prune removes package files nothing accounts for.
	orphan := filepath.Join(dataDir, "bin", "packages", "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runBin(t, "prune", "-f"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphaned package dir not pruned")
	}

	// remove deletes the binary, the package and its links.
	pkgDir := entry().PackageDir
	if err := runBin(t, "remove", tool); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{tool, pkgDir, manPage, completion} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s not removed", p)
		}
	}
	if err := config.CheckAndLoad(); err != nil {
		t.Fatal(err)
	}
	if _, ok := config.Get().Bins[tool]; ok {
		t.Error("entry not removed from config")
	}
}
