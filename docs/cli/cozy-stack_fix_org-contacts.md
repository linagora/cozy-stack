## cozy-stack fix org-contacts

Remove the contacts Sabre no longer has from the organization instance

### Synopsis


This fixer deletes the contacts the twake:contacts:common feed wrote on the
organization instance and did not send again since --since. Run it once a
republication of the domain address books has completed without failures and
the stack has read every message, with --since set a few minutes before the
start of the republication, as the stack and the side service clocks may
differ. Pass the completed task's processedContactCount as --expected-contacts;
and the exact CardDAV paths as a JSON array in --paths-file. The fixer refuses
until the observed paths match this completed snapshot.
Contacts the feed did not write are kept.


```
cozy-stack fix org-contacts <org-id> [flags]
```

### Examples

```
$ cozy-stack fix org-contacts 6740b0e4e0c5c1001f2ef9d1 --since 2026-10-07T08:00:00Z --expected-contacts 100 --paths-file paths.json --dry-run
```

### Options

```
      --dry-run                 Report what would be removed without deleting
      --expected-contacts int   processedContactCount of the completed domain republication (required) (default -1)
      --force                   Do not ask for confirmation before deleting
  -h, --help                    help for org-contacts
      --paths-file string       JSON array of CardDAV paths from the completed republication (required)
      --since string            A few minutes before the republication (RFC 3339)
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

