package cmd

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cozy/cozy-stack/client"
	"github.com/cozy/cozy-stack/client/request"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	humanize "github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

var flagDomainAliases []string
var flagOldDomain string
var flagListFields []string
var flagLocale string
var flagTimezone string
var flagEmail string
var flagPublicName string
var flagPhone string
var flagSettings string
var flagDiskQuota string
var flagApps []string
var flagBlocked bool
var flagBlockingReason string
var flagDeleting bool
var flagDev bool
var flagTrace bool
var flagPassphrase string
var flagForce bool
var flagJSON bool
var flagSwiftLayout int
var flagMigrateTo string
var flagMigrateDryRun bool
var flagMigrateFlagOnly bool
var flagMigrateForce bool
var flagMigratePurgeSource bool
var flagCouchCluster int
var flagUUID string
var flagOIDCID string
var flagFranceConnectID string
var flagMagicLink bool
var flagTOSSigned string
var flagTOS string
var flagTOSLatest string
var flagContextName string
var flagSponsorships []string
var flagOnboardingFinished bool
var flagTTL time.Duration
var flagExpire time.Duration
var flagAllowLoginScope bool
var flagAvailableFields bool
var flagOnboardingSecret string
var flagOnboardingApp string
var flagOnboardingPermissions string
var flagOnboardingState string
var flagPath string

// instanceCmdGroup represents the instances command
var instanceCmdGroup = &cobra.Command{
	Use:     "instances <command>",
	Aliases: []string{"instance"},
	Short:   "Manage instances of a stack",
	Long: `
cozy-stack instances allows to manage the instances of this stack

An instance is a logical space owned by one user and identified by a domain.
For example, bob.cozycloud.cc is the instance of Bob. A single cozy-stack
process can manage several instances.

Each instance has a separate space for storing files and a prefix used to
create its CouchDB databases.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Usage()
	},
}

var showInstanceCmd = &cobra.Command{
	Use:   "show <domain>",
	Short: "Show the instance of the specified domain",
	Long: `
cozy-stack instances show allows to show the instance on the cozy for a
given domain.
`,
	Example: "$ cozy-stack instances show cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Usage()
		}
		domain := args[0]
		ac := newAdminClient()
		in, err := ac.GetInstance(domain)
		if err != nil {
			return err
		}
		json, err := json.MarshalIndent(in, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(json))
		return nil
	},
}

var showDBPrefixInstanceCmd = &cobra.Command{
	Use:   "show-db-prefix <domain>",
	Short: "Show the instance DB prefix of the specified domain",
	Long: `
cozy-stack instances show allows to show the instance prefix on the cozy for a
given domain. The prefix is used for databases and VFS prefixing.

It will also show the couch_cluster if it is not the default one.
`,
	Example: "$ cozy-stack instances show-db-prefix cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Usage()
		}
		domain := args[0]
		ac := newAdminClient()
		in, err := ac.GetInstance(domain)
		if err != nil {
			return err
		}
		if in.Attrs.Prefix != "" {
			fmt.Println(in.Attrs.Prefix)
		} else {
			fmt.Println(couchdb.EscapeCouchdbName(in.Attrs.Domain))
		}
		if in.Attrs.CouchCluster != 0 {
			fmt.Fprintf(os.Stdout, "couch_cluster: %d\n", in.Attrs.CouchCluster)
		}
		return nil
	},
}

var addInstanceCmd = &cobra.Command{
	Use:     "add <domain>",
	Aliases: []string{"create"},
	Short:   "Manage instances of a stack",
	Long: `
cozy-stack instances add allows to create an instance on the cozy for a
given domain.

If the COZY_DISABLE_INSTANCES_ADD_RM env variable is set, creating and
destroying instances will be disabled and the content of this variable will
be used as the error message.
`,
	Example: "$ cozy-stack instances add --passphrase cozy --apps drive,photos,settings,home,store cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		if reason := os.Getenv("COZY_DISABLE_INSTANCES_ADD_RM"); reason != "" {
			return fmt.Errorf("Sorry, instances add is disabled: %s", reason)
		}
		if len(args) == 0 {
			return cmd.Usage()
		}
		if flagDev {
			errPrintfln("The --dev flag has been deprecated")
		}

		var diskQuota int64
		if flagDiskQuota != "" {
			diskQuotaU, err := humanize.ParseBytes(flagDiskQuota)
			if err != nil {
				return err
			}
			diskQuota = int64(diskQuotaU)
		}

		domain := args[0]
		fmt.Fprintf(os.Stdout, "Creating instance for domain \"%s\": please wait...\n", domain)
		ac := newAdminClient()
		in, err := ac.CreateInstance(&client.InstanceOptions{
			Domain:          domain,
			DomainAliases:   flagDomainAliases,
			OldDomain:       flagOldDomain,
			Locale:          flagLocale,
			UUID:            flagUUID,
			OIDCID:          flagOIDCID,
			FranceConnectID: flagFranceConnectID,
			TOSSigned:       flagTOSSigned,
			Timezone:        flagTimezone,
			ContextName:     flagContextName,
			Sponsorships:    flagSponsorships,
			Email:           flagEmail,
			PublicName:      flagPublicName,
			Phone:           flagPhone,
			Settings:        flagSettings,
			SwiftLayout:     flagSwiftLayout,
			CouchCluster:    flagCouchCluster,
			DiskQuota:       diskQuota,
			Apps:            flagApps,
			Passphrase:      flagPassphrase,
			MagicLink:       &flagMagicLink,
			Trace:           &flagTrace,
		})
		if err != nil {
			errPrintfln(
				"Failed to create instance for domain %s", domain)
			return err
		}

		fmt.Fprintf(os.Stdout, "Instance created with success for domain %s\n", in.Attrs.Domain)
		myProtocol := "https"
		if build.IsDevRelease() {
			myProtocol = "http"
		}
		if in.Attrs.RegisterToken != nil {
			fmt.Fprintf(os.Stdout, "Registration token: \"%s\"\n", hex.EncodeToString(in.Attrs.RegisterToken))
			fmt.Fprintf(os.Stdout, "Define your password by visiting %s://%s/?registerToken=%s\n", myProtocol, in.Attrs.Domain, hex.EncodeToString(in.Attrs.RegisterToken))
		}
		if len(flagApps) == 0 {
			return nil
		}
		c, err := ac.NewInstanceClient(domain, consts.Apps)
		if err != nil {
			errPrintfln("Could not generate access to domain %s", domain)
			errPrintfln("%s", err)
			os.Exit(1)
		}
		apps, err := c.ListApps(consts.Apps)
		if err == nil && len(flagApps) != len(apps) {
			for _, slug := range flagApps {
				found := false
				for _, app := range apps {
					if app.Attrs.Slug == slug {
						found = true
						break
					}
				}
				if !found {
					fmt.Fprintf(os.Stdout, "/!\\ Application %s has not been installed\n", slug)
				}
			}
		}
		return nil
	},
}

var migrateStorageCmd = &cobra.Command{
	Use:   "migrate-storage <domain>",
	Short: "Migrate an instance's file storage to another backend (e.g. s3)",
	Long: `cozy-stack instances migrate-storage copies an instance's files, file
versions and avatar to another storage backend and switches the instance to it.
The source data is kept unless --purge-source is given.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cmd.Usage()
		}
		ac := newAdminClient()
		rep, err := ac.MigrateStorage(args[0], client.MigrateStorageOptions{
			To:          flagMigrateTo,
			DryRun:      flagMigrateDryRun,
			FlagOnly:    flagMigrateFlagOnly,
			Force:       flagMigrateForce,
			PurgeSource: flagMigratePurgeSource,
		})
		if err != nil {
			return err
		}
		fmt.Printf("migrated: %d files, %d versions, %d bytes, avatar=%v\n",
			rep.Files, rep.Versions, rep.Bytes, rep.AvatarCopied)
		return nil
	},
}

var modifyInstanceCmd = &cobra.Command{
	Use:   "modify <domain>",
	Short: "Modify the instance properties",
	Long: `
cozy-stack instances modify allows to change the instance properties and
settings for a specified domain.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Usage()
		}

		var diskQuota int64
		if flagDiskQuota != "" {
			diskQuotaU, err := humanize.ParseBytes(flagDiskQuota)
			if err != nil {
				return err
			}
			diskQuota = int64(diskQuotaU)
		}

		domain := args[0]
		ac := newAdminClient()
		opts := &client.InstanceOptions{
			Domain:          domain,
			DomainAliases:   flagDomainAliases,
			OldDomain:       flagOldDomain,
			Locale:          flagLocale,
			UUID:            flagUUID,
			OIDCID:          flagOIDCID,
			FranceConnectID: flagFranceConnectID,
			TOSSigned:       flagTOS,
			TOSLatest:       flagTOSLatest,
			Timezone:        flagTimezone,
			ContextName:     flagContextName,
			Sponsorships:    flagSponsorships,
			Email:           flagEmail,
			PublicName:      flagPublicName,
			Phone:           flagPhone,
			Settings:        flagSettings,
			BlockingReason:  flagBlockingReason,
			DiskQuota:       diskQuota,
			MagicLink:       &flagMagicLink,
		}
		if flag := cmd.Flag("blocked"); flag.Changed {
			opts.Blocked = &flagBlocked
		}
		if flag := cmd.Flag("deleting"); flag.Changed {
			opts.Deleting = &flagDeleting
		}
		if flagOnboardingFinished {
			opts.OnboardingFinished = &flagOnboardingFinished
		}
		in, err := ac.ModifyInstance(opts)
		if err != nil {
			errPrintfln(
				"Failed to modify instance for domain %s", domain)
			return err
		}
		json, err := json.MarshalIndent(in, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(json))
		return nil
	},
}

var updateInstancePassphraseCmd = &cobra.Command{
	Use:     "set-passphrase <domain> <new-passphrase>",
	Short:   "Change the passphrase of the instance",
	Example: "$ cozy-stack instances set-passphrase cozy.localhost:8080 myN3wP4ssowrd!",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return cmd.Usage()
		}
		domain := args[0]
		c := newClient(domain, consts.Settings)
		body := struct {
			New   string `json:"new_passphrase"`
			Force bool   `json:"force"`
		}{
			New:   args[1],
			Force: true,
		}

		reqBody, err := json.Marshal(body)
		if err != nil {
			return err
		}
		res, err := c.Req(&request.Options{
			Method: "PUT",
			Path:   "/settings/passphrase",
			Body:   bytes.NewReader(reqBody),
			Headers: request.Headers{
				"Content-Type": "application/json",
			},
		})
		if err != nil {
			return err
		}

		switch res.StatusCode {
		case http.StatusNoContent:
			fmt.Println("Passphrase has been changed for instance ", domain)
		case http.StatusBadRequest:
			return fmt.Errorf("Bad current passphrase for instance %s", domain)
		case http.StatusInternalServerError:
			return fmt.Errorf("%s", err)
		}

		return nil
	},
}

var quotaInstanceCmd = &cobra.Command{
	Use:   "set-disk-quota <domain> <disk-quota>",
	Short: "Change the disk-quota of the instance",
	Long: `
cozy-stack instances set-disk-quota allows to change the disk-quota of the
instance of the given domain. Set the quota to 0 to remove the quota.
`,
	Example: "$ cozy-stack instances set-disk-quota cozy.localhost:8080 3GB",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return cmd.Usage()
		}
		parsed, err := humanize.ParseBytes(args[1])
		if err != nil {
			return fmt.Errorf("Could not parse disk-quota: %s", err)
		}
		diskQuota := int64(parsed)
		if diskQuota == 0 {
			diskQuota = -1
		}
		domain := args[0]
		ac := newAdminClient()
		_, err = ac.ModifyInstance(&client.InstanceOptions{
			Domain:    domain,
			DiskQuota: diskQuota,
		})
		return err
	},
}

var debugInstanceCmd = &cobra.Command{
	Use:   "debug <true/false>",
	Short: "Activate or deactivate debugging of the instance",
	Long: `
cozy-stack instances debug allows to activate or deactivate the debugging of a
specific domain.
`,
	Example: "$ cozy-stack instances debug --domain cozy.localhost:8080 true",
	RunE: func(cmd *cobra.Command, args []string) error {
		action := "enable"
		domain := flagDomain
		switch len(args) {
		case 0:
			action = "get"
		case 1:
			if strings.ToLower(args[0]) == "false" {
				action = "disable"
			}
		case 2:
			deprecatedDomainArg()
			domain = args[0]
			if strings.ToLower(args[1]) == "false" {
				action = "disable"
			}
		default:
			action = ""
		}
		if action == "" || domain == "" {
			return cmd.Usage()
		}

		ac := newAdminClient()
		var err error
		var debug bool
		switch action {
		case "get":
			debug, err = ac.GetDebug(domain)
		case "enable":
			err = ac.EnableDebug(domain, flagTTL)
			debug = true
		case "disable":
			err = ac.DisableDebug(domain)
			debug = false
		}
		if debug {
			fmt.Fprintf(os.Stdout, "Debug is enabled on %s\n", domain)
		} else {
			fmt.Fprintf(os.Stdout, "Debug is disabled on %s\n", domain)
		}
		return err
	},
}

var countInstanceCmd = &cobra.Command{
	Use:     "count",
	Short:   "Count the instances",
	Example: "$ cozy-stack instances count",
	RunE: func(cmd *cobra.Command, args []string) error {
		ac := newAdminClient()
		count, err := ac.CountInstances()
		if err != nil {
			return err
		}
		if count == 1 {
			fmt.Fprintf(os.Stdout, "%d instance\n", count)
		} else {
			fmt.Fprintf(os.Stdout, "%d instances\n", count)
		}
		return nil
	},
}

var lsInstanceCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List instances",
	Long: `
cozy-stack instances ls allows to list all the instances that can be served
by this server.
`,
	Example: "$ cozy-stack instances ls",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagAvailableFields {
			instance := &client.Instance{}
			val := reflect.ValueOf(instance.Attrs)
			t := val.Type()
			for i := 0; i < t.NumField(); i++ {
				param := t.Field(i).Tag.Get("json")
				param = strings.TrimSuffix(param, ",omitempty")
				param = strings.TrimSuffix(param, ",string")
				fmt.Println(param)
			}
			fmt.Println("db_prefix")
			return nil
		}
		ac := newAdminClient()
		list, err := ac.ListInstances()
		if err != nil {
			return err
		}
		if flagJSON {
			if len(flagListFields) > 0 {
				for _, inst := range list {
					var values map[string]interface{}
					values, err = extractFields(inst.Attrs, flagListFields)
					if err != nil {
						return err
					}

					// Insert the db_prefix value if needed
					for _, v := range flagListFields {
						if v == "db_prefix" {
							values["db_prefix"] = couchdb.EscapeCouchdbName(inst.DBPrefix())
						}
					}

					m := make(map[string]interface{}, len(flagListFields))
					for _, fieldName := range flagListFields {
						if v, ok := values[fieldName]; ok {
							m[fieldName] = v
						} else {
							m[fieldName] = nil
						}
					}

					if err = json.NewEncoder(os.Stdout).Encode(m); err != nil {
						return err
					}
				}
			} else {
				for _, inst := range list {
					if err = json.NewEncoder(os.Stdout).Encode(inst.Attrs); err != nil {
						return err
					}
				}
			}
		} else {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			if len(flagListFields) > 0 {
				format := strings.Repeat("%v\t", len(flagListFields))
				format = format[:len(format)-1] + "\n"
				for _, inst := range list {
					var values map[string]interface{}
					var instancesLines []interface{}

					values, err = extractFields(inst.Attrs, flagListFields)
					if err != nil {
						return err
					}

					// Insert the db_prefix value if needed
					for _, v := range flagListFields {
						if v == "db_prefix" {
							values["db_prefix"] = couchdb.EscapeCouchdbName(inst.DBPrefix())
						}
					}
					// We append to a list to print in the same order as
					// requested
					for _, fieldName := range flagListFields {
						instancesLines = append(instancesLines, values[fieldName])
					}

					fmt.Fprintf(w, format, instancesLines...)
				}
			} else {
				for _, i := range list {
					prefix := i.Attrs.Prefix
					DBPrefix := prefix
					if prefix == "" {
						DBPrefix = couchdb.EscapeCouchdbName(i.Attrs.Domain)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\tv%d\t%s\t%s\n",
						i.Attrs.Domain,
						i.Attrs.Locale,
						formatSize(i.Attrs.BytesDiskQuota),
						formatOnboarded(i),
						i.Attrs.IndexViewsVersion,
						prefix,
						DBPrefix,
					)
				}
			}
			w.Flush()
		}
		return nil
	},
}

func extractFields(data interface{}, fieldsNames []string) (values map[string]interface{}, err error) {
	var m map[string]interface{}
	var b []byte
	b, err = json.Marshal(data)
	if err != nil {
		return
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return
	}
	values = make(map[string]interface{}, len(fieldsNames))
	for _, fieldName := range fieldsNames {
		if v, ok := m[fieldName]; ok {
			values[fieldName] = v
		}
	}
	return
}

func formatSize(size int64) string {
	if size == 0 {
		return "unlimited"
	}
	return humanize.Bytes(uint64(size))
}

func formatOnboarded(i *client.Instance) string {
	if i.Attrs.OnboardingFinished {
		return "onboarded"
	}
	if len(i.Attrs.RegisterToken) > 0 {
		return "onboarding"
	}
	return "pending"
}

var destroyInstanceCmd = &cobra.Command{
	Use:   "destroy <domain>",
	Short: "Remove instance",
	Long: `
cozy-stack instances destroy allows to remove an instance
and all its data.
`,
	Aliases: []string{"rm", "delete", "remove"},
	RunE: func(cmd *cobra.Command, args []string) error {
		if reason := os.Getenv("COZY_DISABLE_INSTANCES_ADD_RM"); reason != "" {
			return fmt.Errorf("Sorry, instances add is disabled: %s", reason)
		}
		if len(args) == 0 {
			return cmd.Usage()
		}

		domain := args[0]

		if !flagForce {
			if err := confirmDomain("remove", domain); err != nil {
				return err
			}
		}

		ac := newAdminClient()
		err := ac.DestroyInstance(domain)
		if err != nil {
			errPrintfln(
				"An error occurred while destroying instance for domain %s", domain)
			return err
		}

		fmt.Fprintf(os.Stdout, "Instance for domain %s has been destroyed with success\n", domain)
		return nil
	},
}

func confirmDomain(action, domain string) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintf(os.Stdout, `Are you sure you want to %s instance for domain %s?
All data associated with this domain will be permanently lost.
Type again the domain to confirm: `, action, domain)

	str, err := reader.ReadString('\n')
	if err != nil {
		return err
	}

	str = strings.ToLower(strings.TrimSpace(str))
	if str != domain {
		return errors.New("Aborted")
	}

	fmt.Println()
	return nil
}

var fsckInstanceCmd = &cobra.Command{
	Use:   "fsck <domain>",
	Short: "Check a vfs",
	Long: `
The cozy-stack fsck command checks that the files in the VFS are not
desynchronized, ie a file present in CouchDB but not swift/localfs, or present
in swift/localfs but not couchdb.

There are 2 steps:

- index integrity checks that there are nothing wrong in the index (CouchDB),
  like a file present in a directory that has been deleted
- files consistency checks that the files are the same in the index (CouchDB)
  and the storage (Swift or localfs).

By default, both operations are done, but you can choose one or the other via
the flags.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		errPrintfln("Please use cozy-stack check fs, this command has been deprecated")
		if len(args) == 0 {
			return cmd.Usage()
		}
		return fsck(args[0])
	},
}

func appOrKonnectorTokenInstance(cmd *cobra.Command, args []string, appType string) error {
	if len(args) < 2 {
		return cmd.Usage()
	}
	ac := newAdminClient()
	token, err := ac.GetToken(&client.TokenOptions{
		Domain:   args[0],
		Subject:  args[1],
		Audience: appType,
		Expire:   &flagExpire,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Println(token)
	return err
}

var appTokenInstanceCmd = &cobra.Command{
	Use:   "token-app <domain> <slug>",
	Short: "Generate a new application token",
	RunE: func(cmd *cobra.Command, args []string) error {
		return appOrKonnectorTokenInstance(cmd, args, "app")
	},
}

var konnectorTokenInstanceCmd = &cobra.Command{
	Use:   "token-konnector <domain> <slug>",
	Short: "Generate a new konnector token",
	RunE: func(cmd *cobra.Command, args []string) error {
		return appOrKonnectorTokenInstance(cmd, args, "konn")
	},
}

var cliTokenInstanceCmd = &cobra.Command{
	Use:   "token-cli <domain> <scopes>",
	Short: "Generate a new CLI access token (global access)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 2 {
			return cmd.Usage()
		}
		ac := newAdminClient()
		token, err := ac.GetToken(&client.TokenOptions{
			Domain:   args[0],
			Scope:    args[1:],
			Audience: consts.CLIAudience,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Println(token)
		return err
	},
}

var oauthTokenInstanceCmd = &cobra.Command{
	Use:     "token-oauth <domain> <clientid> <scopes>",
	Short:   "Generate a new OAuth access token",
	Example: "$ cozy-stack instances token-oauth cozy.localhost:8080 727e677187a51d14ccd59cc0bd000a1d io.cozy.files io.cozy.jobs:POST:sendmail:worker",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 3 {
			return cmd.Usage()
		}
		if args[1] == "" {
			return errors.New("Missing clientID")
		}
		if strings.Contains(args[2], ",") {
			fmt.Fprintf(os.Stderr, "Warning: the delimiter for the scopes is a space!\n")
		}
		ac := newAdminClient()
		token, err := ac.GetToken(&client.TokenOptions{
			Domain:   args[0],
			Subject:  args[1],
			Audience: consts.AccessTokenAudience,
			Scope:    args[2:],
			Expire:   &flagExpire,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Println(token)
		return err
	},
}

var oauthRefreshTokenInstanceCmd = &cobra.Command{
	Use:     "refresh-token-oauth <domain> <clientid> <scopes>",
	Short:   "Generate a new OAuth refresh token",
	Example: "$ cozy-stack instances refresh-token-oauth cozy.localhost:8080 727e677187a51d14ccd59cc0bd000a1d io.cozy.files io.cozy.jobs:POST:sendmail:worker",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 3 {
			return cmd.Usage()
		}
		if strings.Contains(args[2], ",") {
			fmt.Fprintf(os.Stderr, "Warning: the delimiter for the scopes is a space!\n")
		}
		ac := newAdminClient()
		token, err := ac.GetToken(&client.TokenOptions{
			Domain:   args[0],
			Subject:  args[1],
			Audience: consts.RefreshTokenAudience,
			Scope:    args[2:],
		})
		if err != nil {
			return err
		}
		_, err = fmt.Println(token)
		return err
	},
}

var oauthClientInstanceCmd = &cobra.Command{
	Use:   "client-oauth <domain> <redirect_uri> <client_name> <software_id>",
	Short: "Register a new OAuth client",
	Long:  `It registers a new OAuth client and returns its client_id`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 4 {
			return cmd.Usage()
		}
		ac := newAdminClient()
		oauthClient, err := ac.RegisterOAuthClient(&client.OAuthClientOptions{
			Domain:                args[0],
			RedirectURI:           args[1],
			ClientName:            args[2],
			SoftwareID:            args[3],
			AllowLoginScope:       flagAllowLoginScope,
			OnboardingSecret:      flagOnboardingSecret,
			OnboardingApp:         flagOnboardingApp,
			OnboardingPermissions: flagOnboardingPermissions,
			OnboardingState:       flagOnboardingState,
		})
		if err != nil {
			return err
		}
		if flagJSON {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "\t")
			err = encoder.Encode(oauthClient)
		} else {
			_, err = fmt.Println(oauthClient["client_id"])
		}
		return err
	},
}

var findOauthClientCmd = &cobra.Command{
	Use:   "find-oauth-client <domain> <software_id>",
	Short: "Find an OAuth client",
	Long:  `Search an OAuth client from its SoftwareID`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 2 {
			return cmd.Usage()
		}
		var v interface{}
		ac := newAdminClient()

		q := url.Values{
			"domain":      {args[0]},
			"software_id": {args[1]},
		}

		req := &request.Options{
			Method:  "GET",
			Path:    "instances/oauth_client",
			Queries: q,
		}
		res, err := ac.Req(req)
		if err != nil {
			return err
		}
		errd := json.NewDecoder(res.Body).Decode(&v)
		if err != nil {
			return errd
		}
		json, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(json))

		return err
	},
}

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export an instance",
	Long:  `Export the files, documents, and settings`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ac := newAdminClient()
		return ac.Export(&client.ExportOptions{
			Domain:    flagDomain,
			LocalPath: flagPath,
		})
	},
}

var importCmd = &cobra.Command{
	Use:   "import <URL>",
	Short: "Import data from an export link",
	Long:  "This command will reset the Cozy instance and import data from an export link",
	RunE: func(cmd *cobra.Command, args []string) error {
		ac := newAdminClient()
		if len(args) < 1 {
			return errors.New("The URL to the exported data is missing")
		}

		if !flagForce {
			if err := confirmDomain("reset", flagDomain); err != nil {
				return err
			}
		}

		return ac.Import(flagDomain, &client.ImportOptions{
			ManifestURL: args[0],
		})
	},
}

var showSwiftPrefixInstanceCmd = &cobra.Command{
	Use:     "show-swift-prefix <domain>",
	Short:   "Show the instance swift prefix of the specified domain",
	Example: "$ cozy-stack instances show-swift-prefix cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		var v map[string]string

		ac := newAdminClient()
		if len(args) < 1 {
			return errors.New("The domain is missing")
		}

		req := &request.Options{
			Method: "GET",
			Path:   "instances/" + args[0] + "/swift-prefix",
		}
		res, err := ac.Req(req)
		if err != nil {
			return err
		}
		errd := json.NewDecoder(res.Body).Decode(&v)
		if errd != nil {
			return errd
		}
		json, errj := json.MarshalIndent(v, "", "  ")
		if errj != nil {
			return errj
		}
		fmt.Println(string(json))

		return nil
	},
}

var instanceAppVersionCmd = &cobra.Command{
	Use:     "show-app-version [app-slug] [version]",
	Short:   `Show instances that have a particular app version`,
	Example: "$ cozy-stack instances show-app-version drive 1.0.1",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return cmd.Usage()
		}

		ac := newAdminClient()
		path := fmt.Sprintf("/instances/with-app-version/%s/%s", args[0], args[1])
		res, err := ac.Req(&request.Options{
			Method: "GET",
			Path:   path,
		})

		if err != nil {
			return err
		}

		out := struct {
			Instances []string `json:"instances"`
		}{}

		err = json.NewDecoder(res.Body).Decode(&out)
		if err != nil {
			return err
		}
		if len(out.Instances) == 0 {
			return fmt.Errorf("No instances have application \"%s\" in version \"%s\"", args[0], args[1])
		}

		json, err := json.MarshalIndent(out.Instances, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(json))
		return nil
	},
}

var setAuthModeCmd = &cobra.Command{
	Use:     "auth-mode [domain] [auth-mode]",
	Short:   `Set instance auth-mode`,
	Example: "$ cozy-stack instances auth-mode cozy.localhost:8080 two_factor_mail",
	Long: `Change the authentication mode for an instance. Two options are allowed:
- two_factor_mail
- basic
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return cmd.Usage()
		}

		domain := args[0]
		ac := newAdminClient()

		body := struct {
			AuthMode string `json:"auth_mode"`
		}{
			AuthMode: args[1],
		}

		reqBody, err := json.Marshal(body)
		if err != nil {
			return err
		}

		res, err := ac.Req(&request.Options{
			Method: "POST",
			Path:   "/instances/" + url.PathEscape(domain) + "/auth-mode",
			Body:   bytes.NewReader(reqBody),
			Headers: request.Headers{
				"Content-Type": "application/json",
			},
		})
		if err != nil {
			return err
		}
		if res.StatusCode == http.StatusNoContent {
			fmt.Fprintf(os.Stdout, "Auth mode has been changed for %s\n", domain)
		} else {
			resBody, err := io.ReadAll(res.Body)
			if err != nil {
				return err
			}
			fmt.Println(string(resBody))
		}
		return nil
	},
}

var cleanSessionsCmd = &cobra.Command{
	Use:     "clean-sessions <domain>",
	Short:   "Remove the io.cozy.sessions and io.cozy.sessions.logins bases",
	Example: "$ cozy-stack instance clean-sessions cozy.localhost:8080",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Usage()
		}
		domain := args[0]
		ac := newAdminClient()
		return ac.CleanSessions(domain)
	},
}

func init() {
	instanceCmdGroup.AddCommand(showInstanceCmd)
	instanceCmdGroup.AddCommand(showDBPrefixInstanceCmd)
	instanceCmdGroup.AddCommand(addInstanceCmd)
	instanceCmdGroup.AddCommand(migrateStorageCmd)
	instanceCmdGroup.AddCommand(modifyInstanceCmd)
	instanceCmdGroup.AddCommand(countInstanceCmd)
	instanceCmdGroup.AddCommand(lsInstanceCmd)
	instanceCmdGroup.AddCommand(quotaInstanceCmd)
	instanceCmdGroup.AddCommand(debugInstanceCmd)
	instanceCmdGroup.AddCommand(destroyInstanceCmd)
	instanceCmdGroup.AddCommand(fsckInstanceCmd)
	instanceCmdGroup.AddCommand(appTokenInstanceCmd)
	instanceCmdGroup.AddCommand(konnectorTokenInstanceCmd)
	instanceCmdGroup.AddCommand(cliTokenInstanceCmd)
	instanceCmdGroup.AddCommand(oauthTokenInstanceCmd)
	instanceCmdGroup.AddCommand(oauthRefreshTokenInstanceCmd)
	instanceCmdGroup.AddCommand(oauthClientInstanceCmd)
	instanceCmdGroup.AddCommand(findOauthClientCmd)
	instanceCmdGroup.AddCommand(exportCmd)
	instanceCmdGroup.AddCommand(importCmd)
	instanceCmdGroup.AddCommand(showSwiftPrefixInstanceCmd)
	instanceCmdGroup.AddCommand(instanceAppVersionCmd)
	instanceCmdGroup.AddCommand(updateInstancePassphraseCmd)
	instanceCmdGroup.AddCommand(setAuthModeCmd)
	instanceCmdGroup.AddCommand(cleanSessionsCmd)
	addInstanceCmd.Flags().StringSliceVar(&flagDomainAliases, "domain-aliases", nil, "Specify one or more aliases domain for the instance (separated by ',')")
	addInstanceCmd.Flags().StringVar(&flagOldDomain, "old-domain", "", "Old domain of the cozy instance")
	addInstanceCmd.Flags().StringVar(&flagLocale, "locale", consts.DefaultLocale, "Locale of the new cozy instance")
	addInstanceCmd.Flags().StringVar(&flagUUID, "uuid", "", "The UUID of the instance")
	addInstanceCmd.Flags().StringVar(&flagOIDCID, "oidc_id", "", "The identifier for checking authentication from OIDC")
	addInstanceCmd.Flags().StringVar(&flagFranceConnectID, "franceconnect_id", "", "The identifier for checking authentication with FranceConnect")
	addInstanceCmd.Flags().BoolVar(&flagMagicLink, "magic_link", false, "Enable authentication with magic links sent by email")
	addInstanceCmd.Flags().StringVar(&flagTOS, "tos", "", "The TOS version signed")
	addInstanceCmd.Flags().StringVar(&flagTimezone, "tz", "", "The timezone for the user")
	addInstanceCmd.Flags().StringVar(&flagContextName, "context-name", "", "Context name of the instance")
	addInstanceCmd.Flags().StringSliceVar(&flagSponsorships, "sponsorships", nil, "Sponsorships of the instance (comma separated list)")
	addInstanceCmd.Flags().StringVar(&flagEmail, "email", "", "The email of the owner")
	addInstanceCmd.Flags().StringVar(&flagPublicName, "public-name", "", "The public name of the owner")
	addInstanceCmd.Flags().StringVar(&flagPhone, "phone", "", "The phone number of the owner")
	addInstanceCmd.Flags().StringVar(&flagSettings, "settings", "", "A list of settings (eg context:foo,offer:premium)")
	addInstanceCmd.Flags().IntVar(&flagSwiftLayout, "swift-layout", -1, "Specify the layout to use for Swift (from 0 for layout V1 to 2 for layout V3, -1 means the default)")
	migrateStorageCmd.Flags().StringVar(&flagMigrateTo, "to", "s3", "Target storage scheme")
	migrateStorageCmd.Flags().BoolVar(&flagMigrateDryRun, "dry-run", false, "Report what would be copied without writing or switching")
	migrateStorageCmd.Flags().BoolVar(&flagMigrateFlagOnly, "flag-only", false, "Switch the backend pointer without copying (rollback to a retained source)")
	migrateStorageCmd.Flags().BoolVar(&flagMigrateForce, "force", false, "Required with --flag-only; writes since cutover are lost")
	migrateStorageCmd.Flags().BoolVar(&flagMigratePurgeSource, "purge-source", false, "Delete source objects after a successful switch")
	addInstanceCmd.Flags().IntVar(&flagCouchCluster, "couch-cluster", -1, "Specify the CouchDB cluster where the instance will be created (-1 means the default)")
	addInstanceCmd.Flags().StringVar(&flagDiskQuota, "disk-quota", "", "The quota allowed to the instance's VFS")
	addInstanceCmd.Flags().StringSliceVar(&flagApps, "apps", nil, "Apps to be preinstalled")
	addInstanceCmd.Flags().BoolVar(&flagDev, "dev", false, "To create a development instance (deprecated)")
	addInstanceCmd.Flags().BoolVar(&flagTrace, "trace", false, "Show where time is spent")
	addInstanceCmd.Flags().StringVar(&flagPassphrase, "passphrase", "", "Register the instance with this passphrase (useful for tests)")
	modifyInstanceCmd.Flags().StringSliceVar(&flagDomainAliases, "domain-aliases", nil, "Specify one or more aliases domain for the instance (separated by ',')")
	modifyInstanceCmd.Flags().StringVar(&flagOldDomain, "old-domain", "", "Old domain of the cozy instance")
	modifyInstanceCmd.Flags().StringVar(&flagLocale, "locale", "", "New locale")
	modifyInstanceCmd.Flags().StringVar(&flagUUID, "uuid", "", "New UUID")
	modifyInstanceCmd.Flags().StringVar(&flagOIDCID, "oidc_id", "", "New identifier for checking authentication from OIDC")
	modifyInstanceCmd.Flags().StringVar(&flagFranceConnectID, "franceconnect_id", "", "The identifier for checking authentication with FranceConnect")
	modifyInstanceCmd.Flags().BoolVar(&flagMagicLink, "magic_link", false, "Enable authentication with magic links sent by email")
	modifyInstanceCmd.Flags().StringVar(&flagTOS, "tos", "", "Update the TOS version signed")
	modifyInstanceCmd.Flags().StringVar(&flagTOSLatest, "tos-latest", "", "Update the latest TOS version")
	modifyInstanceCmd.Flags().StringVar(&flagTimezone, "tz", "", "New timezone")
	modifyInstanceCmd.Flags().StringVar(&flagContextName, "context-name", "", "New context name")
	modifyInstanceCmd.Flags().StringSliceVar(&flagSponsorships, "sponsorships", nil, "Sponsorships of the instance (comma separated list)")
	modifyInstanceCmd.Flags().StringVar(&flagEmail, "email", "", "New email")
	modifyInstanceCmd.Flags().StringVar(&flagPublicName, "public-name", "", "New public name")
	modifyInstanceCmd.Flags().StringVar(&flagPhone, "phone", "", "New phone number")
	modifyInstanceCmd.Flags().StringVar(&flagSettings, "settings", "", "New list of settings (eg offer:premium)")
	modifyInstanceCmd.Flags().StringVar(&flagDiskQuota, "disk-quota", "", "Specify a new disk quota")
	modifyInstanceCmd.Flags().StringVar(&flagBlockingReason, "blocking-reason", "", "Code that explains why the instance is blocked (PAYMENT_FAILED, LOGIN_FAILED, etc.)")
	modifyInstanceCmd.Flags().BoolVar(&flagBlocked, "blocked", false, "Block the instance")
	modifyInstanceCmd.Flags().BoolVar(&flagDeleting, "deleting", false, "Set (or remove) the deleting flag (ex: `--deleting=false`)")
	modifyInstanceCmd.Flags().BoolVar(&flagOnboardingFinished, "onboarding-finished", false, "Force the finishing of the onboarding")
	destroyInstanceCmd.Flags().BoolVar(&flagForce, "force", false, "Force the deletion without asking for confirmation")
	debugInstanceCmd.Flags().StringVar(&flagDomain, "domain", cozyDomain(), "Specify the domain name of the instance")
	debugInstanceCmd.Flags().DurationVar(&flagTTL, "ttl", 24*time.Hour, "Specify how long the debug mode will last")
	fsckInstanceCmd.Flags().BoolVar(&flagCheckFSIndexIntegrity, "index-integrity", false, "Check the index integrity only")
	fsckInstanceCmd.Flags().BoolVar(&flagCheckFSFilesConsistensy, "files-consistency", false, "Check the files consistency only (between CouchDB and Swift)")
	fsckInstanceCmd.Flags().BoolVar(&flagCheckFSFailFast, "fail-fast", false, "Stop the FSCK on the first error")
	fsckInstanceCmd.Flags().BoolVar(&flagJSON, "json", false, "Output more informations in JSON format")
	oauthClientInstanceCmd.Flags().BoolVar(&flagJSON, "json", false, "Output more informations in JSON format")
	oauthClientInstanceCmd.Flags().BoolVar(&flagAllowLoginScope, "allow-login-scope", false, "Allow login scope")
	oauthClientInstanceCmd.Flags().StringVar(&flagOnboardingSecret, "onboarding-secret", "", "Specify an OnboardingSecret")
	oauthClientInstanceCmd.Flags().StringVar(&flagOnboardingApp, "onboarding-app", "", "Specify an OnboardingApp")
	oauthClientInstanceCmd.Flags().StringVar(&flagOnboardingPermissions, "onboarding-permissions", "", "Specify an OnboardingPermissions")
	oauthClientInstanceCmd.Flags().StringVar(&flagOnboardingState, "onboarding-state", "", "Specify an OnboardingState")
	oauthTokenInstanceCmd.Flags().DurationVar(&flagExpire, "expire", 0, "Make the token expires in this amount of time, as a duration string, e.g. \"1h\"")
	appTokenInstanceCmd.Flags().DurationVar(&flagExpire, "expire", 0, "Make the token expires in this amount of time")
	lsInstanceCmd.Flags().BoolVar(&flagJSON, "json", false, "Show each line as a json representation of the instance")
	lsInstanceCmd.Flags().StringSliceVar(&flagListFields, "fields", nil, "Arguments shown for each line in the list")
	lsInstanceCmd.Flags().BoolVar(&flagAvailableFields, "available-fields", false, "List available fields for --fields option")
	exportCmd.Flags().StringVar(&flagDomain, "domain", "", "Specify the domain name of the instance")
	exportCmd.Flags().StringVar(&flagPath, "path", "", "Specify the local path where to store the export archive")
	importCmd.Flags().StringVar(&flagDomain, "domain", "", "Specify the domain name of the instance")
	importCmd.Flags().BoolVar(&flagForce, "force", false, "Force the import without asking for confirmation")
	_ = exportCmd.MarkFlagRequired("domain")
	_ = importCmd.MarkFlagRequired("domain")
	RootCmd.AddCommand(instanceCmdGroup)
}
