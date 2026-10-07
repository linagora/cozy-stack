## cozy-stack fix org-contacts

Remove the contacts Sabre no longer has from the organization instance

### Synopsis


This fixer deletes the contacts the twake:contacts:common feed wrote on the
organization instance and did not send again since --since. Run it once a
republication of the domain address books has completed without failures and
the stack has read every message, with --since set to the start of the
republication. Contacts the feed did not write are kept.


```
cozy-stack fix org-contacts <org-id> [flags]
```

### Examples

```
$ cozy-stack fix org-contacts 6740b0e4e0c5c1001f2ef9d1 --since 2026-10-07T08:00:00Z --dry-run
```

### Options

```
      --dry-run        Report what would be removed without deleting
      --force          Do not ask for confirmation before deleting
  -h, --help           help for org-contacts
      --since string   Start of the republication (RFC 3339)
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

