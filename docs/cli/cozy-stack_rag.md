## cozy-stack rag

Manage the RAG indexing of an instance

### Synopsis


cozy-stack rag manages the indexing of an instance's files on the openRAG
server used for AI features. The indexing follows the knowledge base folders
of the assistants; these commands are operator tools.


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
* [cozy-stack rag prune](cozy-stack_rag_prune.md)	 - Delete from openRAG what no knowledge base folder claims
* [cozy-stack rag purge](cozy-stack_rag_purge.md)	 - Delete everything openRAG holds for the instance
* [cozy-stack rag reconcile](cozy-stack_rag_reconcile.md)	 - Re-index the subtree of a knowledge base folder (or of all of them)
* [cozy-stack rag reset](cozy-stack_rag_reset.md)	 - Restart the indexing from the beginning of the changes feed

