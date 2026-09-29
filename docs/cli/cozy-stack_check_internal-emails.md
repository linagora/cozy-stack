## cozy-stack check internal-emails

Check the internal emails of all the instances

### Synopsis


This command checks that the internal email of each instance matches its
settings email. The drifts are logged by the stack, not fixed.


```
cozy-stack check internal-emails [flags]
```

### Options

```
  -h, --help   help for internal-emails
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

* [cozy-stack check](cozy-stack_check.md)	 - A set of tools to check that instances are in the expected state.

