package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/spf13/cobra"
)

var flagCheckFSIndexIntegrity bool
var flagCheckFSFilesConsistensy bool
var flagCheckFSFailFast bool
var flagCheckSharingsFast bool

var checkCmdGroup = &cobra.Command{
	Use:   "check <command>",
	Short: "A set of tools to check that instances are in the expected state.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Usage()
	},
}

var checkFSCmd = &cobra.Command{
	Use:   "fs <domain>",
	Short: "Check a vfs",
	Long: `
This command checks that the files in the VFS are not desynchronized, ie a file
present in CouchDB but not swift/localfs, or present in swift/localfs but not
couchdb.

There are 2 steps:

- index integrity checks that there are nothing wrong in the index (CouchDB),
  like a file present in a directory that has been deleted
- files consistency checks that the files are the same in the index (CouchDB)
  and the storage (Swift or localfs).

By default, both operations are done, but you can choose one or the other via
the flags.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Usage()
		}
		return fsck(args[0])
	},
}

func fsck(domain string) error {
	if flagCheckFSFilesConsistensy && flagCheckFSIndexIntegrity {
		flagCheckFSIndexIntegrity = false
		flagCheckFSFilesConsistensy = false
	}

	ac := newAdminClient()
	res, err := ac.Req(&request.Options{
		Method: "GET",
		Path:   "/instances/" + url.PathEscape(domain) + "/fsck",
		Queries: url.Values{
			"IndexIntegrity":   {strconv.FormatBool(flagCheckFSIndexIntegrity)},
			"FilesConsistency": {strconv.FormatBool(flagCheckFSFilesConsistensy)},
			"FailFast":         {strconv.FormatBool(flagCheckFSFailFast)},
		},
	})
	if err != nil {
		return err
	}

	hasLogs := false
	scanner := bufio.NewScanner(res.Body)
	buf := make([]byte, 512*1024) // The default buffer can be too short for some lines
	scanner.Buffer(buf, len(buf))

	for scanner.Scan() {
		hasLogs = true
		fmt.Println(string(scanner.Bytes()))
	}
	if err := scanner.Err(); err != nil {
		_ = res.Body.Close()
		return err
	}

	if hasLogs {
		// Needs to handle manually the body close because os.Exit bypass all the
		// defer functions.
		_ = res.Body.Close()
		os.Exit(1)
	}

	_ = res.Body.Close()
	return nil
}

var checkTriggers = &cobra.Command{
	Use:   "triggers <domain>",
	Short: "Check the triggers",
	Long: `
This command checks that the instance doesn't have duplicate triggers: several
triggers of the same type, for the same worker, and with the same arguments.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Usage()
		}
		domain := args[0]

		ac := newAdminClient()
		res, err := ac.Req(&request.Options{
			Method: "POST",
			Path:   "/instances/" + url.PathEscape(domain) + "/checks/triggers",
		})
		if err != nil {
			return err
		}

		var result []map[string]interface{}
		err = json.NewDecoder(res.Body).Decode(&result)
		if err != nil {
			return err
		}

		if len(result) > 0 {
			for _, r := range result {
				j, _ := json.Marshal(r)
				fmt.Fprintf(os.Stdout, "%s\n", j)
			}
			os.Exit(1)
		}
		return nil
	},
}

var checkSharedCmd = &cobra.Command{
	Use:   "shared <domain>",
	Short: "Check the io.cozy.shared documents",
	Long: `
The io.cozy.shared documents have a tree of revisions. This command will check
that all revisions in this tree are either the root or their parent have a
generation smaller than their generation.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Usage()
		}
		domain := args[0]

		ac := newAdminClient()
		res, err := ac.Req(&request.Options{
			Method: "POST",
			Path:   "/instances/" + url.PathEscape(domain) + "/checks/shared",
		})
		if err != nil {
			return err
		}

		var result []map[string]interface{}
		err = json.NewDecoder(res.Body).Decode(&result)
		if err != nil {
			return err
		}

		if len(result) > 0 {
			for _, r := range result {
				j, _ := json.Marshal(r)
				fmt.Fprintf(os.Stdout, "%s\n", j)
			}
			os.Exit(1)
		}
		return nil
	},
}

var checkSharingsCmd = &cobra.Command{
	Use:   "sharings <domain>",
	Short: "Check the io.cozy.sharings documents",
	Long: `
This command checks that the io.cozy.sharings have no inconsistencies. It can
be triggers that are missing on an active sharing, or missing credentials for
an active member.

There are 2 steps:

- setup integrity checks that there are nothing wrong in the configuration like
  a missing trigger
- files and folders consistency checks that the shared documents are the same
  for all members

By default, both operations are done, but you can choose to skip the consistency
check via the flags.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Usage()
		}
		domain := args[0]

		ac := newAdminClient()
		res, err := ac.Req(&request.Options{
			Method: "POST",
			Path:   "/instances/" + url.PathEscape(domain) + "/checks/sharings",
			Queries: url.Values{
				"SkipFSConsistency": {strconv.FormatBool(flagCheckSharingsFast)},
			},
		})
		if err != nil {
			return err
		}

		var result []map[string]interface{}
		err = json.NewDecoder(res.Body).Decode(&result)
		if err != nil {
			return err
		}

		if len(result) > 0 {
			for _, r := range result {
				j, _ := json.Marshal(r)
				fmt.Fprintf(os.Stdout, "%s\n", j)
			}
			os.Exit(1)
		}
		return nil
	},
}

var checkInternalEmailsCmd = &cobra.Command{
	Use:   "internal-emails",
	Short: "Check the internal emails of all the instances",
	Long: `
This command checks that the internal email of each instance matches its
settings email. The drifts are logged by the stack, not fixed.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ac := newAdminClient()
		res, err := ac.Req(&request.Options{
			Method: "POST",
			Path:   "/instances/checks/internal-emails",
		})
		if err != nil {
			return err
		}
		defer res.Body.Close()

		var check lifecycle.InternalEmailsCheck
		if err := json.NewDecoder(res.Body).Decode(&check); err != nil {
			return err
		}
		for _, e := range check.Errors {
			fmt.Fprintf(os.Stderr, "Error: %s\n", e)
		}
		fmt.Fprintf(os.Stdout, "Scanned: %d\nDrifts: %d\nErrors: %d\n",
			check.Scanned, check.Drifts, len(check.Errors))
		if check.Drifts > 0 || len(check.Errors) > 0 {
			return fmt.Errorf("%d drifts and %d errors", check.Drifts, len(check.Errors))
		}
		return nil
	},
}

func init() {
	checkCmdGroup.AddCommand(checkFSCmd)
	checkCmdGroup.AddCommand(checkTriggers)
	checkCmdGroup.AddCommand(checkSharedCmd)
	checkCmdGroup.AddCommand(checkSharingsCmd)
	checkCmdGroup.AddCommand(checkInternalEmailsCmd)
	checkFSCmd.Flags().BoolVar(&flagCheckFSIndexIntegrity, "index-integrity", false, "Check the index integrity only")
	checkFSCmd.Flags().BoolVar(&flagCheckFSFilesConsistensy, "files-consistency", false, "Check the files consistency only (between CouchDB and Swift)")
	checkFSCmd.Flags().BoolVar(&flagCheckFSFailFast, "fail-fast", false, "Stop the FSCK on the first error")
	checkSharingsCmd.Flags().BoolVar(&flagCheckSharingsFast, "fast", false, "Skip the sharings FS consistency check")

	RootCmd.AddCommand(checkCmdGroup)
}
