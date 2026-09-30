## cozy-stack fix emails

Backfill the email of all the instances

### Synopsis


This fixer sets the email of each instance from its settings email.
It skips organization instances and keeps an email already set. Emails
found on several instances are listed for manual resolution and left unset.
If a settings email can't be read, nothing is written and the command fails.


```
cozy-stack fix emails [flags]
```

### Examples

```
$ cozy-stack fix emails --dry-run
```

### Options

```
      --dry-run   Report what would change without writing
      --force     Do not ask for confirmation before writing
  -h, --help      help for emails
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

