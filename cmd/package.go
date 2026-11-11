package cmd

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/caarlos0/log"
	"github.com/marcosnils/bin/pkg/assets"
	"github.com/marcosnils/bin/pkg/config"
	"github.com/marcosnils/bin/pkg/providers"
)

// installResult describes what an install wrote to disk, so it can be
// recorded in the config and cleaned up later.
type installResult struct {
	hash       []byte
	packageDir string
	links      []string
}

// apply copies the install result into a config entry and records the
// mode that was actually installed.
func (r *installResult) apply(b *config.Binary) *config.Binary {
	b.Hash = fmt.Sprintf("%x", r.hash)
	b.PackageDir = r.packageDir
	b.Links = r.links
	b.Mode = config.ModeBinary
	if r.packageDir != "" {
		b.Mode = config.ModeFull
	}
	return b
}

// installBinary installs the fetched file at path. With full set and an
// archive available, the whole package is installed (see installPackage);
// otherwise only the binary is written. prev is the existing config entry
// for path, if any, so files from a previous package install get cleaned up.
func installBinary(f *providers.File, path string, overwrite, full bool, prev *config.Binary) (*installResult, error) {
	if full && len(f.Archive) > 0 {
		return installPackage(f, path, overwrite, prev)
	}
	if full {
		log.Infof("%s is not an archive, installing the binary only", f.Name)
	}

	hash, err := saveToDisk(f, path, overwrite)
	if err != nil {
		return nil, err
	}
	if prev != nil {
		removePackage(prev, false)
	}
	return &installResult{hash: hash}, nil
}

// dataHome returns the base directory for user data files, following the
// XDG base directory specification.
func dataHome() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(d) {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return d, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

// shareDir returns the data directory for binaries installed in binDir.
// A bin dir uses the share dir of its tree, so the default tree's bin dir
// uses the tree's share dir, ~/.local/bin uses ~/.local/share and
// /usr/local/bin uses /usr/local/share. That's also where man and the
// shells look for man pages and completions of binaries in PATH. Other dirs,
// and ~/bin (so bin never creates ~/share), use the user's data home.
func shareDir(binDir string) (string, error) {
	abs, err := filepath.Abs(config.ExpandPath(binDir))
	if err != nil {
		return "", err
	}
	if filepath.Base(abs) == "bin" {
		tree := filepath.Dir(abs)
		if home, err := os.UserHomeDir(); err != nil || tree != home {
			return filepath.Join(tree, "share"), nil
		}
	}
	return dataHome()
}

// packagesRoot is the directory holding one extracted package per binary.
func packagesRoot(share string) string {
	return filepath.Join(share, "bin", "packages")
}

// isPackagesRoot reports whether dir looks like a packagesRoot, as a
// safeguard before deleting anything below it.
func isPackagesRoot(dir string) bool {
	return filepath.Base(dir) == "packages" && filepath.Base(filepath.Dir(dir)) == "bin"
}

// installPackage extracts the whole release archive into its own directory
// under packagesRoot, points path at the binary inside it with a symlink and
// links the package's man pages and shell completions into the standard
// per-user locations. An existing package is replaced only once the new one
// is fully extracted.
func installPackage(f *providers.File, path string, overwrite bool, prev *config.Binary) (*installResult, error) {
	epath, err := filepath.Abs(os.ExpandEnv(path))
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(f.Data)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)

	if _, err := os.Lstat(epath); err == nil && !overwrite {
		return nil, fmt.Errorf("%w", os.ErrExist)
	}

	share, err := shareDir(filepath.Dir(epath))
	if err != nil {
		return nil, err
	}
	root := packagesRoot(share)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	name := filepath.Base(epath)
	pkgDir := filepath.Join(root, name)
	for _, b := range config.Get().Bins {
		if b.PackageDir == pkgDir && os.ExpandEnv(b.Path) != epath {
			return nil, fmt.Errorf("package directory %s is already used by %s", pkgDir, b.Path)
		}
	}

	rel, err := locateBinary(f.Archive, data, f.PackagePath, f.Name)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp(root, "."+name+".new-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	// MkdirTemp creates the dir as 0700, but the package must be readable
	// by everyone who can run the binary.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return nil, err
	}

	log.Infof("Extracting %s@%s into %s", f.Name, f.Version, pkgDir)
	if err := extractArchive(f.Archive, tmp); err != nil {
		return nil, err
	}
	// The binary must be executable even if the archive didn't say so.
	if err := os.Chmod(filepath.Join(tmp, rel), 0o755); err != nil {
		return nil, err
	}

	// Swap the new package into place, keeping the old one until the
	// binary is linked so we can roll back.
	old := filepath.Join(root, "."+name+".old")
	_ = os.RemoveAll(old)
	hadOld := false
	if _, err := os.Lstat(pkgDir); err == nil {
		if err := os.Rename(pkgDir, old); err != nil {
			return nil, err
		}
		hadOld = true
	}
	rollback := func() {
		_ = os.RemoveAll(pkgDir)
		if hadOld {
			_ = os.Rename(old, pkgDir)
		}
	}
	if err := os.Rename(tmp, pkgDir); err != nil {
		rollback()
		return nil, err
	}

	target := filepath.Join(pkgDir, rel)
	if err := replaceWithSymlink(target, epath); err != nil {
		// Symlinks may not be available (e.g. on Windows without the
		// required privilege); copy the binary instead.
		log.Warnf("Could not link %s to %s (%v), copying the binary instead", epath, target, err)
		if _, err := saveToDisk(&providers.File{Data: bytes.NewReader(data), Name: f.Name, Version: f.Version}, epath, true); err != nil {
			rollback()
			return nil, err
		}
	}
	_ = os.RemoveAll(old)

	// Links from the previous install point into pkgDir and are recreated
	// below for the new package contents.
	if prev != nil {
		removeLinks(prev.Links, pkgDir)
	}
	links, err := linkSupportFiles(share, pkgDir, f.Archive, strings.TrimSuffix(name, ".exe"))
	if err != nil {
		return nil, err
	}

	return &installResult{hash: hash[:], packageDir: pkgDir, links: links}, nil
}

// replaceWithSymlink atomically points dst at target.
func replaceWithSymlink(target, dst string) error {
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".new")
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// archivePath turns an archive entry name into a relative path that's safe
// to extract, rejecting absolute paths and paths escaping the package dir.
func archivePath(name string) (string, error) {
	p := filepath.FromSlash(path.Clean(strings.TrimPrefix(name, "./")))
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("refusing to extract unsafe archive path %q", name)
	}
	return p, nil
}

// locateBinary finds the archive entry holding the selected binary.
func locateBinary(entries []assets.ArchiveEntry, data []byte, packagePath, name string) (string, error) {
	var match string
	for _, e := range entries {
		if e.Linkname != "" || !bytes.Equal(e.Data, data) {
			continue
		}
		if e.Name == packagePath || match == "" || path.Base(e.Name) == name && path.Base(match) != name {
			match = e.Name
		}
		if e.Name == packagePath {
			break
		}
	}
	if match == "" {
		return "", errors.New("could not find the binary in the package")
	}
	return archivePath(match)
}

// extractArchive writes the archive entries into dir. Writes go through an
// os.Root so neither entry names nor symlinks can escape dir, and symlinks
// whose target lies outside the package are skipped.
func extractArchive(entries []assets.ArchiveEntry, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	// Regular files first, so no file is ever written through a symlink
	// created from the archive.
	for _, e := range entries {
		if e.Linkname != "" {
			continue
		}
		p, err := archivePath(e.Name)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		// Readable by everyone, writable only by the owner, like binaries.
		mode := (e.Mode.Perm() | 0o644) &^ 0o022
		_ = root.Remove(p)
		fh, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		_, err = fh.Write(e.Data)
		if cerr := fh.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if err := root.Chmod(p, mode); err != nil {
			return err
		}
	}

	for _, e := range entries {
		if e.Linkname == "" {
			continue
		}
		p, err := archivePath(e.Name)
		if err != nil {
			return err
		}
		if path.IsAbs(e.Linkname) || !filepath.IsLocal(filepath.Join(filepath.Dir(p), filepath.FromSlash(e.Linkname))) {
			log.Warnf("Skipping symlink %s pointing outside the package (%s)", e.Name, e.Linkname)
			continue
		}
		if err := root.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		_ = root.Remove(p)
		if err := root.Symlink(e.Linkname, p); err != nil {
			return err
		}
	}
	return nil
}

var manPageRe = regexp.MustCompile(`^[^.].*\.([1-9])[a-z]*(\.gz)?$`)

// manSection returns the man section of an archive entry, or "" if the entry
// isn't a man page. Besides the file name, the content must look like roff
// (or be gzipped) so that e.g. versioned shared libraries aren't picked up.
func manSection(e assets.ArchiveEntry) string {
	base := path.Base(e.Name)
	m := manPageRe.FindStringSubmatch(base)
	if m == nil || e.Linkname != "" || e.Mode&0o111 != 0 || strings.Contains(base, ".so.") || len(e.Data) == 0 {
		return ""
	}
	gz := strings.HasSuffix(base, ".gz")
	if gz && !bytes.HasPrefix(e.Data, []byte{0x1f, 0x8b}) || !gz && e.Data[0] != '.' && e.Data[0] != '\'' {
		return ""
	}
	return m[1]
}

var completionSuffixes = []string{".bash-completion", ".bash_completion", ".bash", ".zsh", ".fish"}

// completionStem strips the shell-specific parts of a completion file name,
// e.g. "_tool", "tool.bash" and "tool-completion.zsh" all become "tool".
func completionStem(base string) string {
	for _, s := range completionSuffixes {
		base = strings.TrimSuffix(base, s)
	}
	base = strings.TrimPrefix(base, "_")
	for _, s := range []string{"-completion", "_completion", ".completion"} {
		base = strings.TrimSuffix(base, s)
	}
	return base
}

// completionShell returns the shell an archive entry is a completion script
// for, or "" if it isn't one. Generic script extensions only count when the
// file lives in a completion directory or is named after the command.
func completionShell(name, cmd string) string {
	base := path.Base(name)
	dir := strings.ToLower(path.Dir(name))
	ofCmd := completionStem(base) == cmd
	inCompDir := strings.Contains(dir, "complet")
	if !ofCmd && !inCompDir {
		return ""
	}

	switch {
	case strings.HasSuffix(base, ".bash"), strings.HasSuffix(base, ".bash-completion"), strings.HasSuffix(base, ".bash_completion"):
		return "bash"
	case strings.HasSuffix(base, ".zsh"), strings.HasPrefix(base, "_") && !strings.Contains(base, "."):
		return "zsh"
	case strings.HasSuffix(base, ".fish"):
		return "fish"
	case !strings.Contains(base, "."):
		// e.g. completions/bash/tool
		for _, shell := range []string{"bash", "zsh", "fish"} {
			if strings.Contains(path.Base(dir), shell) {
				return shell
			}
		}
	}
	return ""
}

// completionDest is where a completion for cmd is linked for each shell,
// relative to the data home. These locations are picked up automatically by
// bash-completion and fish; zsh needs the directory in its fpath.
func completionDest(shell, cmd string) string {
	switch shell {
	case "bash":
		return filepath.Join("bash-completion", "completions", cmd)
	case "zsh":
		return filepath.Join("zsh", "site-functions", "_"+cmd)
	default:
		return filepath.Join("fish", "vendor_completions.d", cmd+".fish")
	}
}

// supportFiles maps link destinations (relative to the data home) to the
// archive entries providing them.
func supportFiles(entries []assets.ArchiveEntry, cmd string) map[string]string {
	files := map[string]string{}
	completions := map[string][]string{}

	for _, e := range entries {
		if e.Linkname != "" {
			continue
		}
		if sec := manSection(e); sec != "" {
			dest := filepath.Join("man", "man"+sec, path.Base(e.Name))
			if _, ok := files[dest]; !ok {
				files[dest] = e.Name
			}
			continue
		}
		if shell := completionShell(e.Name, cmd); shell != "" {
			completions[shell] = append(completions[shell], e.Name)
		}
	}

	for shell, names := range completions {
		sort.Strings(names)
		var named []string
		for _, n := range names {
			if completionStem(path.Base(n)) == cmd {
				named = append(named, n)
			}
		}
		switch {
		case len(named) > 0:
			files[completionDest(shell, cmd)] = named[0]
		case len(names) == 1:
			files[completionDest(shell, cmd)] = names[0]
		default:
			log.Warnf("Found several %s completions (%s), not installing any", shell, strings.Join(names, ", "))
		}
	}
	return files
}

// linkSupportFiles links the man pages and shell completions found in the
// package into the per-user data directories and returns the created links.
// Existing files that bin doesn't own are left alone.
func linkSupportFiles(share, pkgDir string, entries []assets.ArchiveEntry, cmd string) ([]string, error) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}

	files := supportFiles(entries, cmd)
	dests := make([]string, 0, len(files))
	for d := range files {
		dests = append(dests, d)
	}
	sort.Strings(dests)

	var links []string
	for _, d := range dests {
		src, err := archivePath(files[d])
		if err != nil {
			return links, err
		}
		dst := filepath.Join(share, d)
		if _, err := os.Lstat(dst); err == nil {
			if !ownedLink(dst, pkgDir) {
				log.Warnf("%s already exists and is not managed by bin, skipping", dst)
				continue
			}
			_ = os.Remove(dst)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return links, err
		}
		if err := os.Symlink(filepath.Join(pkgDir, src), dst); err != nil {
			return links, err
		}
		log.Infof("Linked %s", dst)
		links = append(links, dst)
		if strings.HasPrefix(d, "zsh") {
			log.Infof("Add %s to your zsh fpath to enable the completion", filepath.Dir(dst))
		}
	}
	return links, nil
}

// ownedLink reports whether p is a symlink into pkgDir.
func ownedLink(p, pkgDir string) bool {
	t, err := os.Readlink(p)
	return err == nil && strings.HasPrefix(t, pkgDir+string(filepath.Separator))
}

func removeLinks(links []string, pkgDir string) {
	for _, l := range links {
		if ownedLink(l, pkgDir) {
			_ = os.Remove(l)
		}
	}
}

// removePackage deletes the package directory and support file links of a
// package install. With binary set, the binary link is removed as well if it
// still points into the package. Paths outside packagesRoot are never
// deleted, whatever the config says.
func removePackage(b *config.Binary, binary bool) {
	if b.PackageDir == "" {
		return
	}
	removeLinks(b.Links, b.PackageDir)
	if p := os.ExpandEnv(b.Path); binary && ownedLink(p, b.PackageDir) {
		_ = os.Remove(p)
	}
	if !isPackagesRoot(filepath.Dir(b.PackageDir)) {
		log.Warnf("Not removing %s since it's not in a bin packages directory", b.PackageDir)
		return
	}
	if err := os.RemoveAll(b.PackageDir); err != nil {
		log.Warnf("Could not remove %s: %v", b.PackageDir, err)
	}
}

// supportLinkDirs are the directories, relative to a share dir, that
// support file links are created in (see supportFiles).
func supportLinkDirs() []string {
	dirs := []string{
		filepath.Join("bash-completion", "completions"),
		filepath.Join("zsh", "site-functions"),
		filepath.Join("fish", "vendor_completions.d"),
	}
	for i := 1; i <= 9; i++ {
		dirs = append(dirs, filepath.Join("man", fmt.Sprintf("man%d", i)))
	}
	return dirs
}

// orphanedPackageFiles finds package files no entry in bins accounts for:
// package dirs no entry references, leftovers from interrupted installs and
// symlinks into those. They're left behind e.g. when an older bin, which
// doesn't know about packages, updated a package install. Binary links in
// bin dirs are only reported when no entry is installed at their path.
// all is used to find the share and bin dirs to look in; it's usually a
// superset of bins.
func orphanedPackageFiles(all map[string]*config.Binary, bins map[string]*config.Binary, defaultPath string) []string {
	referenced := map[string]bool{}
	binPaths := map[string]bool{}
	for _, b := range bins {
		if b.PackageDir != "" {
			referenced[b.PackageDir] = true
		}
		binPaths[os.ExpandEnv(b.Path)] = true
	}

	shares := map[string]bool{}
	binDirs := map[string]bool{}
	addBinDir := func(dir string) {
		dir = os.ExpandEnv(dir)
		binDirs[dir] = true
		if s, err := shareDir(dir); err == nil {
			shares[s] = true
		}
	}
	if defaultPath != "" {
		addBinDir(defaultPath)
	}
	for _, b := range all {
		addBinDir(filepath.Dir(b.Path))
		if b.PackageDir != "" && isPackagesRoot(filepath.Dir(b.PackageDir)) {
			shares[filepath.Dir(filepath.Dir(filepath.Dir(b.PackageDir)))] = true
		}
	}

	var orphans []string
	// orphanLink reports whether p is a symlink into an unreferenced
	// package below root.
	orphanLink := func(p, root string) bool {
		t, err := os.Readlink(p)
		if err != nil || !strings.HasPrefix(t, root+string(filepath.Separator)) {
			return false
		}
		rel := strings.TrimPrefix(t, root+string(filepath.Separator))
		return !referenced[filepath.Join(root, strings.SplitN(rel, string(filepath.Separator), 2)[0])]
	}
	scan := func(dir, root string, skip map[string]bool) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.Type()&os.ModeSymlink != 0 && !skip[p] && orphanLink(p, root) {
				orphans = append(orphans, p)
			}
		}
	}

	for _, share := range sortedKeys(shares) {
		root := packagesRoot(share)
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			p := filepath.Join(root, e.Name())
			if !referenced[p] {
				orphans = append(orphans, p)
			}
		}
		for _, d := range supportLinkDirs() {
			scan(filepath.Join(share, d), root, nil)
		}
		for _, d := range sortedKeys(binDirs) {
			scan(d, root, binPaths)
		}
	}
	return orphans
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
