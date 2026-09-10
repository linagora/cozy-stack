## cozy-stack rag prune

Delete from openRAG what no knowledge base folder claims

```
cozy-stack rag prune <domain> [flags]
```

### Examples

```
$ cozy-stack rag prune cozy.localhost:8080
```

### Options

```
  -h, --help   help for prune
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

