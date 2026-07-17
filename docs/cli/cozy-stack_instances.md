## cozy-stack instances

Manage instances of a stack

### Synopsis


cozy-stack instances allows to manage the instances of this stack

An instance is a logical space owned by one user and identified by a domain.
For example, bob.cozycloud.cc is the instance of Bob. A single cozy-stack
process can manage several instances.

Each instance has a separate space for storing files and a prefix used to
create its CouchDB databases.


```
cozy-stack instances <command> [flags]
```

### Options

```
  -h, --help   help for instances
```

### Options inherited from parent commands

```
      --admin-host string   administration server host (default "localhost")
      --admin-port int      administration server port (default 6060)
  -c, --config string       configuration file (default "$HOME/.cozy.yaml")
      --host string         server host (default "localhost")
  -p, --port int            server port (default 8080)
```

### SEE ALSO

* [cozy-stack](cozy-stack.md)	 - cozy-stack is the main command
* [cozy-stack instances add](cozy-stack_instances_add.md)	 - Manage instances of a stack
* [cozy-stack instances auth-mode](cozy-stack_instances_auth-mode.md)	 - Set instance auth-mode
* [cozy-stack instances clean-sessions](cozy-stack_instances_clean-sessions.md)	 - Remove the io.cozy.sessions and io.cozy.sessions.logins bases
* [cozy-stack instances client-oauth](cozy-stack_instances_client-oauth.md)	 - Register a new OAuth client
* [cozy-stack instances count](cozy-stack_instances_count.md)	 - Count the instances
* [cozy-stack instances debug](cozy-stack_instances_debug.md)	 - Activate or deactivate debugging of the instance
* [cozy-stack instances destroy](cozy-stack_instances_destroy.md)	 - Remove instance
* [cozy-stack instances export](cozy-stack_instances_export.md)	 - Export an instance
* [cozy-stack instances find-oauth-client](cozy-stack_instances_find-oauth-client.md)	 - Find an OAuth client
* [cozy-stack instances fsck](cozy-stack_instances_fsck.md)	 - Check a vfs
* [cozy-stack instances import](cozy-stack_instances_import.md)	 - Import data from an export link
* [cozy-stack instances ls](cozy-stack_instances_ls.md)	 - List instances
* [cozy-stack instances migrate-storage](cozy-stack_instances_migrate-storage.md)	 - Migrate an instance's file storage to another backend (e.g. s3)
* [cozy-stack instances modify](cozy-stack_instances_modify.md)	 - Modify the instance properties
* [cozy-stack instances refresh-token-oauth](cozy-stack_instances_refresh-token-oauth.md)	 - Generate a new OAuth refresh token
* [cozy-stack instances set-disk-quota](cozy-stack_instances_set-disk-quota.md)	 - Change the disk-quota of the instance
* [cozy-stack instances set-passphrase](cozy-stack_instances_set-passphrase.md)	 - Change the passphrase of the instance
* [cozy-stack instances show](cozy-stack_instances_show.md)	 - Show the instance of the specified domain
* [cozy-stack instances show-app-version](cozy-stack_instances_show-app-version.md)	 - Show instances that have a particular app version
* [cozy-stack instances show-db-prefix](cozy-stack_instances_show-db-prefix.md)	 - Show the instance DB prefix of the specified domain
* [cozy-stack instances show-swift-prefix](cozy-stack_instances_show-swift-prefix.md)	 - Show the instance swift prefix of the specified domain
* [cozy-stack instances token-app](cozy-stack_instances_token-app.md)	 - Generate a new application token
* [cozy-stack instances token-cli](cozy-stack_instances_token-cli.md)	 - Generate a new CLI access token (global access)
* [cozy-stack instances token-konnector](cozy-stack_instances_token-konnector.md)	 - Generate a new konnector token
* [cozy-stack instances token-oauth](cozy-stack_instances_token-oauth.md)	 - Generate a new OAuth access token

