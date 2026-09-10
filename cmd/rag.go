package cmd

import (
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/spf13/cobra"
)

var ragDirIDFlag string

var ragCmdGroup = &cobra.Command{
	Use:   "rag <command>",
	Short: "Manage the RAG indexing of an instance",
	Long: `
cozy-stack rag manages the indexing of an instance's files on the openRAG
server used for AI features. The indexing follows the knowledge base folders
of the assistants; these commands are operator tools.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Usage()
	},
}

var ragResetCmd = &cobra.Command{
	Use:     "reset <domain>",
	Short:   "Restart the indexing from the beginning of the changes feed",
	Example: "$ cozy-stack rag reset cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		return ragAdminPost(fmt.Sprintf("/instances/%s/rag/reset", args[0]), nil)
	},
}

var ragPruneCmd = &cobra.Command{
	Use:     "prune <domain>",
	Short:   "Delete from openRAG what no knowledge base folder claims",
	Example: "$ cozy-stack rag prune cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		return ragAdminPost(fmt.Sprintf("/instances/%s/rag/prune", args[0]), nil)
	},
}

var ragPurgeCmd = &cobra.Command{
	Use:     "purge <domain>",
	Short:   "Delete everything openRAG holds for the instance",
	Example: "$ cozy-stack rag purge cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		return ragAdminPost(fmt.Sprintf("/instances/%s/rag/purge", args[0]), nil)
	},
}

var ragReconcileCmd = &cobra.Command{
	Use:     "reconcile <domain> [--dir-id <id>]",
	Short:   "Re-index the subtree of a knowledge base folder (or of all of them)",
	Example: "$ cozy-stack rag reconcile cozy.localhost:8080 --dir-id 6c36a9ee",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		queries := url.Values{}
		if ragDirIDFlag != "" {
			queries.Set("dir_id", ragDirIDFlag)
		}
		return ragAdminPost(fmt.Sprintf("/instances/%s/rag/reconcile", args[0]), queries)
	},
}

func ragAdminPost(path string, queries url.Values) error {
	ac := newAdminClient()
	res, err := ac.Req(&request.Options{Method: "POST", Path: path, Queries: queries})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if _, err := io.Copy(os.Stdout, res.Body); err != nil {
		return err
	}
	fmt.Println()
	return nil
}

func init() {
	ragReconcileCmd.Flags().StringVar(&ragDirIDFlag, "dir-id", "", "only reconcile this knowledge base folder")
	ragCmdGroup.AddCommand(ragResetCmd, ragPruneCmd, ragPurgeCmd, ragReconcileCmd)
	RootCmd.AddCommand(ragCmdGroup)
}
