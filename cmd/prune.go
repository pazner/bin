package cmd

import (
	"os"

	"github.com/caarlos0/log"
	"github.com/marcosnils/bin/pkg/config"
	"github.com/marcosnils/bin/pkg/prompt"
	"github.com/spf13/cobra"
)

type pruneCmd struct {
	cmd  *cobra.Command
	opts pruneOpts
}

type pruneOpts struct {
	force bool
}

func newPruneCmd() *pruneCmd {
	root := &pruneCmd{}
	// nolint: dupl
	cmd := &cobra.Command{
		Use:           "prune",
		Short:         "Prunes binaries that no longer exist in the system and leftover package files",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Get()

			pathsToDel := []string{}
			remaining := map[string]*config.Binary{}
			for p, b := range cfg.Bins {
				ep := os.ExpandEnv(b.Path)
				if _, err := os.Stat(ep); os.IsNotExist(err) {
					log.Infof("%s not found removing", ep)
					pathsToDel = append(pathsToDel, b.Path)
					continue
				}
				remaining[p] = b
			}

			// Package files no remaining entry accounts for: packages of
			// the entries pruned above and leftovers from interrupted
			// installs or older bin versions.
			orphans := orphanedPackageFiles(cfg.Bins, remaining, cfg.BinDir())
			for _, o := range orphans {
				log.Infof("%s is not managed by any binary, removing", o)
			}

			if len(pathsToDel) == 0 && len(orphans) == 0 {
				return nil
			}

			if !root.opts.force {
				err := prompt.Confirm("The following paths will be removed. Continue?")
				if err != nil {
					return err
				}
			}

			for _, o := range orphans {
				if err := os.RemoveAll(o); err != nil {
					log.Warnf("Could not remove %s: %v", o, err)
				}
			}
			return config.RemoveBinaries(pathsToDel)
		},
	}

	root.cmd = cmd
	root.cmd.Flags().BoolVarP(&root.opts.force, "force", "f", false, "Bypass confirmation prompt")
	return root
}
