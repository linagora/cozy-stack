## cozy-stack fix member-copies

Remove the members copied into the instances of an organization

### Synopsis


This fixer deletes directory copies once centralized_org_directory is enabled.
Populate the organization directory first, from B2B events or the contacts service.
Every candidate must have a matching organization contact before any deletion.
Instances retaining replication are skipped. Personal contacts, contacts written
for sharing and feed-owned contacts are kept. It is safe to run twice.


```
cozy-stack fix member-copies <org-id> [flags]
```

### Examples

```
$ cozy-stack fix member-copies 6740b0e4e0c5c1001f2ef9d1 --dry-run
```

### Options

```
      --dry-run   Report what would be removed without deleting
      --force     Do not ask for confirmation before deleting
  -h, --help      help for member-copies
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

* [cozy-stack fix](cozy-stack_fix.md)	 - A set of tools to fix issues or migrate content.

