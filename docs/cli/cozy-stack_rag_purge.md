## cozy-stack rag purge

Delete from openRAG the files no rag-index trigger claims

### Synopsis


List the files openRAG holds for the instance and delete those that no
longer exist (or are trashed) in the Cozy, and those outside every folder
covered by a rag-index trigger when there is no global trigger.


```
cozy-stack rag purge <domain> [flags]
```

### Examples

```
$ cozy-stack rag purge cozy.localhost:8080
```

### Options

```
  -h, --help   help for purge
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

