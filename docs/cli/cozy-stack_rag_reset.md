## cozy-stack rag reset

Restart the indexing from the beginning of the changes feed

```
cozy-stack rag reset <domain> [flags]
```

### Examples

```
$ cozy-stack rag reset cozy.localhost:8080
```

### Options

```
  -h, --help   help for reset
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

* [cozy-stack rag](cozy-stack_rag.md)	 - Manage the RAG indexing of an instance

