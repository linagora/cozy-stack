## cozy-stack instances migrate-storage

Migrate an instance's file storage to another backend (e.g. s3)

### Synopsis

cozy-stack instances migrate-storage copies an instance's files, file
versions and avatar to another storage backend and switches the instance to it.
The source data is kept unless --purge-source is given.

```
cozy-stack instances migrate-storage <domain> [flags]
```

### Options

```
      --dry-run        Report what would be copied without writing or switching
      --flag-only      Switch the backend pointer without copying (rollback to a retained source)
      --force          Required with --flag-only; writes since cutover are lost
  -h, --help           help for migrate-storage
      --purge-source   Delete source objects after a successful switch
      --to string      Target storage scheme (default "s3")
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

* [cozy-stack instances](cozy-stack_instances.md)	 - Manage instances of a stack

