package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var flagSearchAll bool

var searchCmdGroup = &cobra.Command{
	Use:   "search <command>",
	Short: "Manage the server-side file search of instances",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Usage()
	},
}

var searchEnableCmd = &cobra.Command{
	Use:   "enable [domain]",
	Short: "Turn the file search on for an instance, or all of them with --all",
	Long: `
cozy-stack search enable creates the search indices if needed, adds the trigger
that indexes the file changes of the instance, and pushes the first indexing
job, which indexes all the files of an instance that was never indexed.
`,
	Example: "$ cozy-stack search enable alice.cozy.localhost\n$ cozy-stack search enable --all",
	RunE: func(cmd *cobra.Command, args []string) error {
		ac := newAdminClient()
		if !flagSearchAll {
			if len(args) != 1 {
				return cmd.Usage()
			}
			return ac.SearchEnable(args[0])
		}
		if len(args) != 0 {
			return cmd.Usage()
		}
		list, err := ac.ListInstances()
		if err != nil {
			return err
		}
		var errs error
		for _, inst := range list {
			domain := inst.Attrs.Domain
			if err := ac.SearchEnable(domain); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %s\n", domain, err)
				errs = errors.Join(errs, fmt.Errorf("%s: %w", domain, err))
				continue
			}
			fmt.Println(domain)
		}
		return errs
	},
}

var searchResetCmd = &cobra.Command{
	Use:     "reset <domain>",
	Short:   "Drop the search documents of an instance and index it again",
	Example: "$ cozy-stack search reset alice.cozy.localhost",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		return newAdminClient().SearchReset(args[0])
	},
}

var searchStatusCmd = &cobra.Command{
	Use:     "status <domain>",
	Short:   "Show if the search trigger exists, and the last change indexed",
	Example: "$ cozy-stack search status alice.cozy.localhost",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		status, err := newAdminClient().SearchStatus(args[0])
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	},
}

func init() {
	searchEnableCmd.Flags().BoolVar(&flagSearchAll, "all", false, "Enable search for all the instances")
	searchCmdGroup.AddCommand(searchEnableCmd)
	searchCmdGroup.AddCommand(searchResetCmd)
	searchCmdGroup.AddCommand(searchStatusCmd)
	RootCmd.AddCommand(searchCmdGroup)
}
