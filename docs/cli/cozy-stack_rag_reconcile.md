## cozy-stack rag reconcile

Re-index the subtree of a knowledge base folder (or of all of them)

```
cozy-stack rag reconcile <domain> [--dir-id <id>] [flags]
```

### Examples

```
$ cozy-stack rag reconcile cozy.localhost:8080 --dir-id 6c36a9ee
```

### Options

```
      --dir-id string   only reconcile this knowledge base folder
  -h, --help            help for reconcile
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

