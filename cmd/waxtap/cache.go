package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxtap/v3/internal/diskcache"
)

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect or clear the on-disk cache directory",
		Long: "Manage the WaxTap cache directory.\n\n" +
			"WaxTap persists YouTube's player JS (base.js) here so a fresh run can\n" +
			"compile the cipher from disk instead of re-downloading several megabytes.\n" +
			"Entries are size-capped and schema-versioned. `cache clean` removes WaxTap's\n" +
			"own entries and the directory when that empties it; it never removes\n" +
			"anything else, so it is safe to run any time. WaxTap re-fetches whatever it\n" +
			"needs. Disable persistence with --no-cache.",
		// A bare cache command prints help, but an unknown subcommand is a usage
		// error rather than a successful help request.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usagef("unknown cache subcommand %q; expected dir or clean", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newCacheDirCmd(), newCacheCleanCmd())
	// Config flags must be persistent so cache dir and cache clean inherit them.
	bindConfigFlags(cmd.PersistentFlags())
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
			exists, _, populated := diskcache.Describe(dir)
			if outputFlags(cmd).json {
				return writeJSON(cmd.OutOrStdout(), struct {
					SchemaVersion int    `json:"schemaVersion"`
					Dir           string `json:"dir"`
					// Exists keeps its original meaning, the path is there at
					// all, since consumers read it at this schema version.
					Exists bool `json:"exists"`
					// Populated says a WaxTap cache is there, which is what
					// `cache clean` would remove.
					Populated bool `json:"populated"`
				}{schemaVersion, dir, exists, populated})
			}
			fmt.Fprintln(cmd.OutOrStdout(), dir)
			return nil
		},
	}
}

func newCacheCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove WaxTap's cached entries",
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
			exists, isDir, _ := diskcache.Describe(dir)
			if exists && !isDir {
				return usagef("%s is not a directory; --cache-dir names the cache directory", dir)
			}
			removed, err := diskcache.Clean(dir)
			if err != nil {
				return fmt.Errorf("remove cache %s: %w", dir, err)
			}
			if outputFlags(cmd).json {
				return writeJSON(cmd.OutOrStdout(), struct {
					SchemaVersion int    `json:"schemaVersion"`
					Dir           string `json:"dir"`
					Removed       bool   `json:"removed"`
				}{schemaVersion, dir, removed})
			}
			switch {
			case removed:
				// "cleaned", not "removed <dir>": the entries always go, and
				// the directory itself only when that emptied it.
				fmt.Fprintf(cmd.OutOrStdout(), "cleaned %s\n", dir)
			case !exists:
				fmt.Fprintf(cmd.OutOrStdout(), "nothing to clean (%s does not exist)\n", dir)
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "nothing to clean (%s holds no WaxTap cache)\n", dir)
			}
			return nil
		},
	}
}
