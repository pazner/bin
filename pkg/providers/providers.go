package providers

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/marcosnils/bin/pkg/assets"
)

var ErrInvalidProvider = errors.New("invalid provider")

type File struct {
	Data        io.Reader
	Name        string
	Version     string
	Length      int64
	PackagePath string
	// SelectedAsset is the top-level release asset name that was chosen,
	// stored so the same artefact can be defaulted on the next upgrade
	SelectedAsset string
	// Archive holds every entry of the release archive the binary came
	// from. It's only populated when FetchOpts.Full is set and the release
	// asset is an archive.
	Archive []assets.ArchiveEntry
}

func (f *File) Hash() ([]byte, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f.Data); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

type FetchOpts struct {
	All            bool
	PackageName    string
	PackagePath    string
	SkipPatchCheck bool
	Version        string
	NamePattern    string
	// PreviousAsset is the top-level asset name selected on the previous
	// install/upgrade, and PreviousVersion is the version it was selected at.
	// On upgrades these let bin re-select the same artefact across versions.
	PreviousAsset   string
	PreviousVersion string
	// AutoSelectPrevious selects the asset matching PreviousAsset without
	// prompting when exactly one candidate matches. Used by `ensure`, which
	// re-installs an already-chosen artefact at a known version.
	AutoSelectPrevious bool
	// Full keeps the whole release archive (see File.Archive) so the
	// package's support files can be installed alongside the binary.
	Full bool
}

type Provider interface {
	// Fetch returns the file metadata to retrieve a specific binary given
	// for a provider
	Fetch(*FetchOpts) (*File, error)
	// GetLatestVersion returns the version and the URL of the
	// latest version for this binary
	GetLatestVersion() (string, string, error)

	// GetID returns the unique identiifer of this provider
	GetID() string
}

var (
	httpUrlPrefix      = regexp.MustCompile("^https?://")
	dockerUrlPrefix    = regexp.MustCompile("^docker://")
	goinstallUrlPrefix = regexp.MustCompile("^goinstall://")
)

func New(u, provider string) (Provider, error) {
	if dockerUrlPrefix.MatchString(u) {
		return newDocker(u)
	}
	if goinstallUrlPrefix.MatchString(u) || provider == "goinstall" {
		return newGoInstall(u)
	}
	if provider == "local" || isLocalPath(u) {
		return newLocal(u)
	}
	if !httpUrlPrefix.MatchString(u) {
		u = fmt.Sprintf("https://%s", u)
	}

	purl, err := url.Parse(u)
	if err != nil {
		return nil, err
	}

	if strings.Contains(purl.Host, "github") || provider == "github" {
		return newGitHub(purl)
	}

	if strings.Contains(purl.Host, "gitlab") || provider == "gitlab" {
		return newGitLab(purl)
	}

	if strings.Contains(purl.Host, "codeberg") || provider == "codeberg" {
		return newCodeberg(purl)
	}

	if strings.Contains(purl.Host, "get.helm.sh") || provider == "helm" {
		return newHelm(purl)
	}

	if strings.Contains(purl.Host, "releases.hashicorp.com") || provider == "hashicorp" {
		return newHashiCorp(purl)
	}

	return nil, fmt.Errorf("Can't find provider for url %s", u)
}

// URLCanonicalizer is implemented by providers whose URL is stored in the
// config in a normalized form, e.g. a relative path as an absolute URL.
type URLCanonicalizer interface {
	CanonicalURL() string
}

// BinaryPathSetter is implemented by providers that need the path of the
// binary already installed on disk, e.g. to read metadata from it before
// checking for updates.
type BinaryPathSetter interface {
	SetBinaryPath(path string)
}
