package providers

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/caarlos0/log"
	"github.com/marcosnils/bin/pkg/assets"
	"github.com/marcosnils/bin/pkg/config"
)

// ErrSourceMissing is returned when the source of a binary doesn't exist,
// e.g. a local file that was moved or a config synced from another machine.
var ErrSourceMissing = errors.New("source not found")

// local installs binaries and release archives from the local filesystem.
type local struct {
	path string
}

// isLocalPath reports whether u refers to the local filesystem: a file://
// URL or an absolute, relative or home-relative path.
func isLocalPath(u string) bool {
	for _, prefix := range []string{"file://", "/", "./", "../", "~/"} {
		if strings.HasPrefix(u, prefix) {
			return true
		}
	}
	if runtime.GOOS == "windows" {
		return filepath.VolumeName(u) != "" || strings.HasPrefix(u, `.\`) || strings.HasPrefix(u, `..\`) || strings.HasPrefix(u, `~\`)
	}
	return false
}

func newLocal(u string) (*local, error) {
	p := u
	if strings.HasPrefix(u, "file://") {
		pu, err := url.Parse(u)
		if err != nil {
			return nil, err
		}
		if pu.Host != "" && pu.Host != "localhost" {
			return nil, fmt.Errorf("file URL %s must not have a host", u)
		}
		p = filepath.FromSlash(pu.Path)
		// file:///C:/dir on Windows
		if runtime.GOOS == "windows" && len(p) > 2 && p[0] == '\\' && p[2] == ':' {
			p = p[1:]
		}
	}
	abs, err := filepath.Abs(config.ExpandPath(p))
	if err != nil {
		return nil, err
	}
	return &local{path: abs}, nil
}

func (l *local) GetID() string {
	return "local"
}

// CanonicalURL is the absolute file:// URL of the source, so the config
// works regardless of the directory bin runs in.
func (l *local) CanonicalURL() string {
	p := filepath.ToSlash(l.path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func (l *local) read() ([]byte, error) {
	fi, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrSourceMissing, l.path)
	} else if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory, only files can be installed", l.path)
	}
	return os.ReadFile(l.path)
}

// versionInName matches a version in a file name, e.g. v1.4.2 in
// tool-v1.4.2-darwin-arm64.tar.gz.
var versionInName = regexp.MustCompile(`(?:^|[-_.])(v?\d+\.\d+(?:\.\d+)?(?:-(?:rc|alpha|beta|pre)\.?\d*)?)(?:[-_.]|$)`)

// localVersion is the version in the file name if there's one, or a short
// hash of the content otherwise, so that updates pick up rebuilt files.
func localVersion(name string, data []byte) string {
	if m := versionInName.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:4])
}

func (l *local) Fetch(opts *FetchOpts) (*File, error) {
	data, err := l.read()
	if err != nil {
		return nil, err
	}
	name := filepath.Base(l.path)
	version := localVersion(name, data)
	if opts.Version != "" && opts.Version != version {
		log.Warnf("%s is at version %s now, installing it instead of %s", l.path, version, opts.Version)
	}
	log.Infof("Reading %s", l.path)

	f := assets.NewFilter(&assets.FilterOpts{SkipScoring: opts.All, PackagePath: opts.PackagePath, SkipPathCheck: opts.SkipPatchCheck, PackageName: opts.PackageName, NamePattern: opts.NamePattern, PreferredAsset: opts.PreviousAsset, PreferredVersion: opts.PreviousVersion, CurrentVersion: version, AutoSelectPreferred: opts.AutoSelectPrevious, KeepArchive: opts.Full})

	// The file is the only asset; filtering it applies the asset part of
	// the name pattern, so the file part applies to the archive contents.
	if _, err := f.FilterAssets(name, []*assets.Asset{{Name: name, URL: l.CanonicalURL()}}); err != nil {
		return nil, err
	}

	outFile, err := f.ProcessFile(name, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	return &File{Data: outFile.Source, Name: outFile.Name, Version: version, PackagePath: outFile.PackagePath, SelectedAsset: name, Archive: outFile.Archive}, nil
}

func (l *local) GetLatestVersion() (string, string, error) {
	data, err := l.read()
	if err != nil {
		return "", "", err
	}
	return localVersion(filepath.Base(l.path), data), l.CanonicalURL(), nil
}
