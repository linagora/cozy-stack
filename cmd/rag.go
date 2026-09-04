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
server used for AI features. The indexing itself is driven by rag-index
triggers created by the apps; these commands are operator tools.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Usage()
	},
}

var ragResetCmd = &cobra.Command{
	Use:   "reset <domain>",
	Short: "Restart the indexing from the beginning of the changes feed",
	Long: `
Delete the checkpoint of the rag-index triggers of the instance and launch
them, so the whole changes feed is scanned again. With --dir-id, only the
trigger scoped to that folder is reset.
`,
	Example: "$ cozy-stack rag reset cozy.localhost:8080 --dir-id 6c36a9ee",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		queries := url.Values{}
		if ragDirIDFlag != "" {
			queries.Set("dir_id", ragDirIDFlag)
		}
		return ragAdminPost(fmt.Sprintf("/instances/%s/rag/reset", args[0]), queries)
	},
}

var ragPurgeCmd = &cobra.Command{
	Use:   "purge <domain>",
	Short: "Delete from openRAG the files no rag-index trigger claims",
	Long: `
List the files openRAG holds for the instance and delete those that no
longer exist (or are trashed) in the Cozy, and those outside every folder
covered by a rag-index trigger when there is no global trigger.
`,
	Example: "$ cozy-stack rag purge cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		return ragAdminPost(fmt.Sprintf("/instances/%s/rag/purge", args[0]), nil)
	},
}

func ragAdminPost(path string, queries url.Values) error {
	ac := newAdminClient()
	res, err := ac.Req(&request.Options{
		Method:  "POST",
		Path:    path,
		Queries: queries,
	})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, err = io.Copy(os.Stdout, res.Body)
	fmt.Println()
	return err
}

func init() {
	ragResetCmd.Flags().StringVar(&ragDirIDFlag, "dir-id", "", "only reset the trigger scoped to this folder")
	ragCmdGroup.AddCommand(ragResetCmd)
	ragCmdGroup.AddCommand(ragPurgeCmd)
	RootCmd.AddCommand(ragCmdGroup)
}
