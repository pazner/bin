package providers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalVersion(t *testing.T) {
	for name, want := range map[string]string{
		"tool-v1.4.2-darwin-arm64.tar.gz": "v1.4.2",
		"tool_1.4.2_linux_amd64.zip":      "1.4.2",
		"tool-2.0.tar.gz":                 "2.0",
		"tool-v1.4.2-rc.1-linux":          "v1.4.2-rc.1",
		"tool-linux-x86_64":               "sha256:2cf24dba",
		"python3.11-tool":                 "sha256:2cf24dba",
	} {
		if got := localVersion(name, []byte("hello")); got != want {
			t.Errorf("localVersion(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestNewLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix paths")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd, _ := os.Getwd()

	cases := map[string]string{
		"/opt/my tools/tool":          "file:///opt/my%20tools/tool",
		"file:///opt/my%20tools/tool": "file:///opt/my%20tools/tool",
		"./dist/tool":                 "file://" + filepath.Join(wd, "dist", "tool"),
		"~/dist/tool":                 "file://" + filepath.Join(home, "dist", "tool"),
	}
	for u, want := range cases {
		p, err := New(u, "")
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		l, ok := p.(*local)
		if !ok {
			t.Fatalf("%s: got provider %s, want local", u, p.GetID())
		}
		if got := l.CanonicalURL(); got != want {
			t.Errorf("%s: canonical URL %q, want %q", u, got, want)
		}
		// The canonical URL round-trips.
		again, err := newLocal(l.CanonicalURL())
		if err != nil || again.path != l.path {
			t.Errorf("%s: canonical URL resolves to %q, %v; want %q", u, again.path, err, l.path)
		}
	}

	if _, err := New("file://host/tool", ""); err == nil {
		t.Error("expected an error for a file URL with a host")
	}
	// A bare name isn't a path.
	if p, err := New("github.com/owner/repo", ""); err != nil || p.GetID() != "github" {
		t.Errorf("bare repo resolved to %v, %v", p, err)
	}
}

func TestLocalFetch(t *testing.T) {
	dir := t.TempDir()

	// A plain binary: the version comes from its content.
	bin := filepath.Join(dir, "tool")
	if err := os.WriteFile(bin, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := newLocal(bin)
	if err != nil {
		t.Fatal(err)
	}
	v1, u, err := l.GetLatestVersion()
	if err != nil || u != l.CanonicalURL() {
		t.Fatalf("GetLatestVersion: %q, %q, %v", v1, u, err)
	}
	f, err := l.Fetch(&FetchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(f.Data); string(b) != "v1" || f.Name != "tool" || f.Version != v1 {
		t.Errorf("unexpected file %+v with data %q", f, b)
	}

	// Rebuilding the binary changes its version.
	if err := os.WriteFile(bin, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if v2, _, _ := l.GetLatestVersion(); v2 == v1 {
		t.Errorf("version didn't change with the content: %s", v2)
	}

	// An archive is unpacked and kept with Full.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, mode := range map[string]int64{"tool-v1.2.0/tool": 0o755, "tool-v1.2.0/tool.1": 0o644} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(name))})
		_, _ = tw.Write([]byte(name))
	}
	_ = tw.Close()
	_ = gw.Close()
	archive := filepath.Join(dir, "tool-v1.2.0-linux-amd64.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	l, _ = newLocal(archive)
	f, err = l.Fetch(&FetchOpts{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.Version != "v1.2.0" || f.Name != "tool" || f.PackagePath != "tool-v1.2.0/tool" || len(f.Archive) != 2 || f.SelectedAsset != filepath.Base(archive) {
		t.Errorf("unexpected file %+v", f)
	}

	// Missing sources and directories.
	l, _ = newLocal(filepath.Join(dir, "missing"))
	if _, err := l.Fetch(&FetchOpts{}); !errors.Is(err, ErrSourceMissing) {
		t.Errorf("missing file: got %v, want ErrSourceMissing", err)
	}
	if _, _, err := l.GetLatestVersion(); !errors.Is(err, ErrSourceMissing) {
		t.Errorf("missing file: got %v, want ErrSourceMissing", err)
	}
	l, _ = newLocal(dir)
	if _, err := l.Fetch(&FetchOpts{}); err == nil || errors.Is(err, ErrSourceMissing) {
		t.Errorf("directory: got %v, want an error", err)
	}
}
