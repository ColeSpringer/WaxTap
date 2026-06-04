package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"
)

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect or clear the on-disk cache directory",
		Long: "Manage the WaxTap cache directory.\n\n" +
			"Note: this build caches player programs in memory per invocation; the\n" +
			"on-disk directory is reserved for cached extraction artifacts. `cache\n" +
			"clean` is safe to run any time — WaxTap re-fetches whatever it needs.",
	}
	cmd.AddCommand(newCacheDirCmd(), newCacheCleanCmd())
	return cmd
}

func newCacheDirCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dir",
		Short: "Print the cache directory path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			dir, err := cfg.resolvedCacheDir()
			if err != nil {
				return err
			}
			_, statErr := os.Stat(dir)
			exists := statErr == nil
			if rootFlagsValue.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					SchemaVersion int    `json:"schemaVersion"`
					Dir           string `json:"dir"`
					Exists        bool   `json:"exists"`
				}{schemaVersion, dir, exists})
			}
			fmt.Fprintln(cmd.OutOrStdout(), dir)
			return nil
		},
	}
}

func newCacheCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove the cache directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			dir, err := cfg.resolvedCacheDir()
			if err != nil {
				return err
			}
			_, statErr := os.Stat(dir)
			removed := statErr == nil
			if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove cache %s: %w", dir, err)
			}
			if rootFlagsValue.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					SchemaVersion int    `json:"schemaVersion"`
					Dir           string `json:"dir"`
					Removed       bool   `json:"removed"`
				}{schemaVersion, dir, removed})
			}
			if removed {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", dir)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "nothing to clean (%s does not exist)\n", dir)
			}
			return nil
		},
	}
}
