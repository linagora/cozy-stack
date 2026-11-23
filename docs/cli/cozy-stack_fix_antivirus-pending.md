## cozy-stack fix antivirus-pending

Scan again the files stuck in the antivirus pending status

### Synopsis


This fixer pushes a new antivirus scan for the files whose antivirus status is
still pending although they were last updated more than --older-than ago. Such
a file has no verdict because its scan job failed or was lost, and it stays
blocked when the configuration forbids actions on pending files.

The delay must be longer than a normal scan job can last, retries included, to
avoid scanning twice a file whose scan is still running.


```
cozy-stack fix antivirus-pending <domain> [flags]
```

### Examples

```
$ cozy-stack fix antivirus-pending alice.cozy.localhost --older-than 2h
```

### Options

```
  -h, --help                  help for antivirus-pending
      --older-than duration   Only the files last updated before this delay (default 1h0m0s)
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

