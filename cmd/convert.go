package cmd

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"github.com/caarlos0/log"
	"github.com/marcosnils/bin/pkg/config"
	"github.com/marcosnils/bin/pkg/providers"
	"github.com/spf13/cobra"
)

type convertCmd struct {
	cmd  *cobra.Command
	opts convertOpts
}

type convertOpts struct {
	full            bool
	binary          bool
	dryRun          bool
	continueOnError bool
}

func newConvertCmd() *convertCmd {
	root := &convertCmd{}
	// nolint: dupl
	cmd := &cobra.Command{
		Use:   "convert (--full | --binary) [binary_path]...",
		Short: "Converts binaries between binary-only and full package installs",
		Long: `Converts binaries between binary-only and full package installs, keeping
their installed version. Converting to a full install downloads the release
again; converting to binary-only doesn't. Without arguments, all binaries are
converted.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Get()

			var paths []string
			if len(args) > 0 {
				for _, a := range args {
					p, err := getBinPath(a)
					if err != nil {
						return err
					}
					paths = append(paths, p)
				}
			} else {
				for p := range cfg.Bins {
					paths = append(paths, p)
				}
				sort.Strings(paths)
			}

			var failures []error
			for _, p := range paths {
				if err := convertBinary(cfg.Bins[p], root.opts.full, root.opts.dryRun); err != nil {
					err = fmt.Errorf("error converting %s: %w", p, err)
					if !root.opts.continueOnError {
						return err
					}
					failures = append(failures, err)
				}
			}
			for _, err := range failures {
				log.Warnf("%v", err)
			}
			if len(failures) > 0 {
				return fmt.Errorf("%d of %d conversions failed", len(failures), len(paths))
			}
			return nil
		},
	}

	root.cmd = cmd
	root.cmd.Flags().BoolVarP(&root.opts.full, "full", "F", false, "Convert to full package installs")
	root.cmd.Flags().BoolVarP(&root.opts.binary, "binary", "b", false, "Convert to binary-only installs")
	root.cmd.Flags().BoolVarP(&root.opts.dryRun, "dry-run", "", false, "Only show what would be converted")
	root.cmd.Flags().BoolVarP(&root.opts.continueOnError, "continue-on-error", "c", false, "Continues to convert the next binary if an error is encountered")
	root.cmd.MarkFlagsOneRequired("full", "binary")
	root.cmd.MarkFlagsMutuallyExclusive("full", "binary")
	return root
}

// convertBinary reinstalls b at its current version in the requested mode.
func convertBinary(b *config.Binary, toFull, dryRun bool) error {
	ep := os.ExpandEnv(b.Path)
	want := config.ModeBinary
	if toFull {
		want = config.ModeFull
	}

	if b.IsFull() == toFull {
		if b.Mode == want {
			log.Infof("%s is already a %s install", ep, want)
			return nil
		}
		// The entry has the right layout but a missing or hand-edited
		// mode; only record the mode.
		log.Infof("Recording %s as a %s install", ep, want)
		if dryRun {
			return nil
		}
		b.Mode = want
		return config.UpsertBinary(b)
	}

	if toFull && (b.Provider == "docker" || b.Provider == "goinstall") {
		log.Infof("%s is installed with the %s provider, which has no package to install, skipping", ep, b.Provider)
		return nil
	}

	log.Infof("Converting %s@%s to a %s install", ep, b.Version, want)
	if dryRun {
		return nil
	}

	converted := *b
	if !toFull {
		// The binary is in the package already, no need to download it.
		data, err := os.ReadFile(ep)
		if err != nil {
			return err
		}
		hash, err := saveToDisk(&providers.File{Data: bytes.NewReader(data), Name: b.RemoteName, Version: b.Version}, ep, true)
		if err != nil {
			return err
		}
		removePackage(b, false)
		(&installResult{hash: hash}).apply(&converted)
		return config.UpsertBinary(&converted)
	}

	p, err := providers.New(b.URL, b.Provider)
	if err != nil {
		return err
	}
	pResult, err := p.Fetch(&providers.FetchOpts{
		Version:            b.Version,
		PackageName:        b.RemoteName,
		PackagePath:        b.PackagePath,
		PreviousAsset:      b.SelectedAsset,
		PreviousVersion:    b.Version,
		AutoSelectPrevious: true,
		Full:               true,
	})
	if err != nil {
		return err
	}

	res, err := installBinary(pResult, b.Path, true, true, b)
	if err != nil {
		return err
	}
	res.apply(&converted)
	converted.PackagePath = pResult.PackagePath
	if !converted.IsFull() {
		log.Infof("%s's release isn't an archive, it stays a binary install", ep)
	}
	return config.UpsertBinary(&converted)
}
