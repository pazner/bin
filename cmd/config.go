package cmd

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/caarlos0/log"
	"github.com/marcosnils/bin/pkg/config"
	"github.com/spf13/cobra"
)

type configCmd struct {
	cmd  *cobra.Command
	opts configOpts
}

type configOpts struct {
	setDefaultTree bool
}

func newConfigCmd() *configCmd {
	root := &configCmd{}
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Shows bin's settings and the config file path",
		Long: `Shows bin's settings, their effective values and the path of the config
file. To change a setting, edit the config file. --set-default-tree picks the
default tree interactively, like on the first run.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if root.opts.setDefaultTree {
				tree, err := config.SelectDefaultTree()
				if err != nil {
					return err
				}
				if err := config.SetDefaultTree(tree); err != nil {
					return err
				}
				log.Infof("Default tree set to %s. Binaries installed so far stay where they are", tree)
			}
			return printConfig(os.Stdout)
		},
	}

	root.cmd = cmd
	root.cmd.Flags().BoolVar(&root.opts.setDefaultTree, "set-default-tree", false, "Interactively pick the default tree")
	return root
}

func printConfig(out io.Writer) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	cfg := config.Get()

	orUnset := func(v string) string {
		if v == "" {
			return "(not set)"
		}
		return v
	}
	share, err := shareDir(cfg.BinDir())
	if err != nil {
		return err
	}
	mode := config.ModeBinary
	if cfg.FullByDefault() {
		mode = config.ModeFull
	}

	fmt.Fprintf(out, "Config file: %s\n\n", path)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SETTING\tVALUE\tEFFECTIVE")
	if cfg.DefaultPath != "" {
		fmt.Fprintf(w, "default_path\t%s\t%s (overrides default_tree)\n", cfg.DefaultPath, cfg.BinDir())
	}
	fmt.Fprintf(w, "default_tree\t%s\t%s\n", orUnset(cfg.DefaultTree), config.ExpandPath(cfg.DefaultTree))
	fmt.Fprintf(w, "  binaries\t\t%s\n", cfg.BinDir())
	fmt.Fprintf(w, "  package files\t\t%s\n", share)
	fmt.Fprintf(w, "default_mode\t%s\t%s\n", orUnset(cfg.DefaultMode), mode)
	return w.Flush()
}
