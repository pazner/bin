package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/caarlos0/log"
	"github.com/marcosnils/bin/pkg/options"
)

var cfg config

// currentVersion is the version of the config format, see migrate.
const currentVersion = 1

type config struct {
	// Version is the config format version. Configs written before
	// versions existed have none and are upgraded on load (see migrate).
	Version int `json:"version,omitempty"`
	// DefaultTree is the prefix bin installs into: binaries go into its
	// bin dir and package files into its share dir. It may contain
	// environment variables or a leading ~, see ExpandPath.
	DefaultTree string `json:"default_tree,omitempty"`
	// DefaultPath is the bin dir used by configs from before DefaultTree
	// existed. It's only kept when it can't be expressed as a tree (it
	// isn't named "bin"), and then takes precedence over DefaultTree.
	DefaultPath string `json:"default_path,omitempty"`
	// DefaultMode is the install mode used when `bin install` isn't given
	// --full or --full=false: ModeFull (the default) or ModeBinary.
	DefaultMode string             `json:"default_mode,omitempty"`
	Bins        map[string]*Binary `json:"bins"`
}

// Install modes recorded in Binary.Mode.
const (
	// ModeBinary installs only the binary.
	ModeBinary = "binary"
	// ModeFull installs the whole release archive (see `bin install --full`).
	ModeFull = "full"
)

// ExpandPath expands environment variables and a leading ~ in a path
// from the config.
func ExpandPath(p string) string {
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	return p
}

// BinDir is the directory binaries are installed into by default.
func (c *config) BinDir() string {
	if c.DefaultPath != "" {
		return ExpandPath(c.DefaultPath)
	}
	return filepath.Join(ExpandPath(c.DefaultTree), "bin")
}

// FullByDefault reports whether new installs use ModeFull.
func (c *config) FullByDefault() bool {
	switch c.DefaultMode {
	case "", ModeFull:
		return true
	case ModeBinary:
		return false
	default:
		log.Warnf("Unknown default_mode %q in config, using %q", c.DefaultMode, ModeFull)
		return true
	}
}

type Binary struct {
	Path       string `json:"path"`
	RemoteName string `json:"remote_name"`
	Version    string `json:"version"`
	Hash       string `json:"hash"`
	URL        string `json:"url"`
	Provider   string `json:"provider"`
	// if file is installed from a package format (zip, tar, etc) store
	// the package path in config so we don't ask the user to select
	// the path again when upgrading
	PackagePath string `json:"package_path"`
	// SelectedAsset is the top-level release asset name chosen on install,
	// re-used to default the same artefact when upgrading
	SelectedAsset string `json:"selected_asset"`
	Pinned        bool   `json:"pinned"`
	// Mode is ModeFull when the whole release package was installed and
	// ModeBinary when only the binary was. Entries from before modes
	// existed get ModeBinary when the config is upgraded (see migrate).
	Mode string `json:"mode,omitempty"`
	// PackageDir is where the package contents were extracted to. The
	// binary at Path is a symlink into it.
	PackageDir string `json:"package_dir,omitempty"`
	// Links are the symlinks bin created for the package's man pages and
	// shell completions, removed again on update and removal.
	Links []string `json:"links,omitempty"`
}

// IsFull reports whether the whole release package is installed.
func (b *Binary) IsFull() bool {
	return b.Mode == ModeFull
}

// Path returns the path of the config file.
func Path() (string, error) {
	return getConfigPath()
}

func CheckAndLoad() error {
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	confDir := filepath.Dir(configPath)

	if err := os.Mkdir(confDir, 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("Error creating config directory [%v]", err)
	}

	log.Debugf("Config directory is: %s", confDir)
	f, err := os.OpenFile(configPath, os.O_RDWR|os.O_CREATE, 0664)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	defer f.Close()

	cfg = config{}
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		if err == io.EOF {
			// Empty file and/or was just created, initialize cfg.Bins
			cfg.Bins = map[string]*Binary{}
		} else {
			return err
		}
	}

	changed := migrate(&cfg)

	if len(cfg.DefaultTree) == 0 && len(cfg.DefaultPath) == 0 {
		if cfg.DefaultTree, err = SelectDefaultTree(); err != nil {
			return err
		}
		changed = true
	}

	if cfg.Bins == nil {
		cfg.Bins = map[string]*Binary{}
	}

	if changed {
		if err := write(); err != nil {
			return err
		}
	}

	log.Debugf("Download path set to %s", cfg.BinDir())
	return nil
}

// migrate upgrades a config written by an older bin version to the current
// format and reports whether anything changed:
//   - entries without an install mode were installed binary-only
//   - default_path becomes default_tree, the parent of the bin dir. A
//     default_path not named "bin" can't be expressed as a tree and is kept.
func migrate(c *config) bool {
	if c.Version >= currentVersion {
		return false
	}

	for _, b := range c.Bins {
		if b.Mode == "" {
			b.Mode = ModeBinary
		}
	}

	if c.DefaultTree == "" && c.DefaultPath != "" {
		p := strings.TrimRight(c.DefaultPath, `/\`)
		if filepath.Base(ExpandPath(p)) == "bin" {
			c.DefaultTree = filepath.Dir(p)
			c.DefaultPath = ""
		} else {
			log.Warnf("default_path %s isn't a bin directory, keeping it. Set default_tree instead to also choose where package files go", c.DefaultPath)
		}
	}

	log.Debugf("Upgraded config from version %d to %d", c.Version, currentVersion)
	c.Version = currentVersion
	return true
}

// SelectDefaultTree asks the user for the default tree, offering the
// parents of the writable bin directories in PATH, or asking for a
// directory if there are none. The tree's bin dir is created if needed.
func SelectDefaultTree() (string, error) {
	tree, err := pickTreeFromPath()
	if err != nil {
		log.Info("Could not find a bin directory in PATH automatically, falling back to manual selection")
		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Printf("\nPlease specify the directory to install into (binaries go into its bin directory): ")
			response, err := reader.ReadString('\n')
			if err != nil {
				return "", fmt.Errorf("Invalid input")
			}
			response = strings.TrimSpace(response)

			if err = checkDirExistsAndWritable(ExpandPath(response)); err != nil {
				log.Infof("Can't use %s: %v", response, err)
				// Keep looping until writable and existing dir is selected
				continue
			}
			tree = response
			break
		}
	}

	if err := os.MkdirAll(filepath.Join(ExpandPath(tree), "bin"), 0o755); err != nil {
		return "", err
	}
	return tree, nil
}

// pickTreeFromPath lets the user pick a tree among the parents of the
// writable directories named "bin" in PATH.
func pickTreeFromPath() (string, error) {
	penv := os.Getenv("PATH")
	log.Debugf("User PATH is [%s]", penv)
	seen := map[string]bool{}
	opts := []fmt.Stringer{}
	for _, p := range filepath.SplitList(penv) {
		if filepath.Base(p) != "bin" || seen[p] {
			continue
		}
		seen[p] = true
		if err := checkDirExistsAndWritable(p); err != nil {
			log.Debugf("Error [%s] checking path %s", err, p)
			continue
		}
		log.Debugf("%s seems to be a dir and writable, adding option.", p)
		opts = append(opts, options.LiteralStringer(filepath.Dir(p)))
	}

	if len(opts) == 0 {
		return "", errors.New("Automatic path detection didn't return any results")
	}

	choice, err := options.SelectCustom("Pick a default install tree (binaries go into its bin directory): ", opts)
	if err != nil {
		return "", err
	}
	return choice.(fmt.Stringer).String(), nil
}

// SetDefaultTree stores tree as the default tree.
func SetDefaultTree(tree string) error {
	cfg.DefaultTree = tree
	cfg.DefaultPath = ""
	return write()
}

func Get() *config {
	return &cfg
}

// UpsertBinary adds or updats an existing
// binary resource in the config
func UpsertBinary(c *Binary) error {
	if c != nil {
		cfg.Bins[c.Path] = c
		err := write()
		if err != nil {
			return err
		}
	}

	return nil
}

// RemoveBinaries removes the specified paths
// from bin configuration. It doesn't care about the order
func RemoveBinaries(paths []string) error {
	for _, p := range paths {
		delete(cfg.Bins, p)
	}

	return write()
}

func write() error {
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	f, err := os.OpenFile(configPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0664)
	if err != nil {
		return err
	}

	defer f.Close()

	decoder := json.NewEncoder(f)
	decoder.SetIndent("", "    ")
	err = decoder.Encode(cfg)
	if err != nil {
		return err
	}

	return nil
}

// GetArch is the running program's operating system target:
// one of darwin, freebsd, linux, and so on.
func GetArch() []string {
	res := []string{runtime.GOARCH}
	if runtime.GOARCH == "amd64" {
		// Adding x86_64 manually since the uname syscall (man 2 uname)
		// is not implemented in all systems
		res = append(res, "x86_64")
		res = append(res, "x64")
	}
	return res
}

// GetOS is the running program's architecture target:
// one of 386, amd64, arm, s390x, and so on.
func GetOS() []string {
	res := []string{runtime.GOOS}
	if runtime.GOOS == "windows" {
		// Adding win since some repositories release with that as the indicator of a windows binary
		res = append(res, "win")
	}
	return res
}

// getConfigPath returns the path to the configuration directory respecting
// the `XDG Base Directory specification` using the following strategy:
//   - honor BIN_CONFIG is set
//   - to prevent breaking of existing configurations, check if "$HOME/.bin/config.json"
//     exists and return "$HOME/.bin"
//   - if "XDG_CONFIG_HOME" is set, return "$XDG_CONFIG_HOME/bin"
//   - if "$HOME/.config" exists, return "$home/.config/bin"
//   - default to "$HOME/.bin/"
//
// ToDo: move the function to config_unix.go and add a similar function for windows,
//
//	%APPDATA% might be the right place on windows
func getConfigPath() (string, error) {

	c := os.Getenv("BIN_CONFIG")
	if len(c) > 0 {
		if _, err := os.Stat(c); !os.IsNotExist(err) {
			return c, nil
		} else {
			return "", err
		}
	}

	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		if _, err := os.Stat(filepath.Join(home, ".bin", "config.json")); !os.IsNotExist(err) {
			return filepath.Join(path.Join(home, ".bin", "config.json")), nil
		}
	}

	c = os.Getenv("XDG_CONFIG_HOME")
	if _, err := os.Stat(c); !os.IsNotExist(err) {
		return filepath.Join(c, "bin", "config.json"), nil
	}

	if homeErr != nil {
		return "", homeErr
	}
	c = filepath.Join(home, ".config")
	if _, err := os.Stat(c); !os.IsNotExist(err) {
		return filepath.Join(c, "bin", "config.json"), nil
	}

	return filepath.Join(home, ".bin", "config.json"), nil
}

func GetOSSpecificExtensions() []string {
	switch runtime.GOOS {
	case "linux":
		return []string{"AppImage"}
	case "windows":
		return []string{"exe"}
	default:
		return nil
	}
}
