## cozy-stack rag

Manage the RAG indexing of an instance

### Synopsis


cozy-stack rag manages the indexing of an instance's files on the openRAG
server used for AI features. The indexing itself is driven by rag-index
triggers created by the apps; these commands are operator tools.


```
cozy-stack rag <command> [flags]
```

### Options

```
  -h, --help   help for rag
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

* [cozy-stack](cozy-stack.md)	 - cozy-stack is the main command
* [cozy-stack rag purge](cozy-stack_rag_purge.md)	 - Delete from openRAG the files no rag-index trigger claims
* [cozy-stack rag reset](cozy-stack_rag_reset.md)	 - Restart the indexing from the beginning of the changes feed

