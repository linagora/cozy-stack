## cozy-stack fix personal-contacts

Publish to Sabre the personal contacts that only exist in the stack

### Synopsis


This fixer publishes on twake:contacts:collected the personal contacts that
have an email and no CardDAV path, with their name, emails and phones. Run it
before configuring contacts_search for a context, as the share suggestions then
come from Sabre only.
Each contact is marked external, so the card coming back from the
twake:contacts:common feed updates it instead of creating a duplicate. From
then on, its name, emails and phones come from Sabre.
Member copies, the user's own contact and trashed contacts are left out.
Instances without common_contacts or an email are skipped. It is safe to run
twice.


```
cozy-stack fix personal-contacts [flags]
```

### Examples

```
$ cozy-stack fix personal-contacts --context linagora_default --dry-run
```

### Options

```
      --context string   Only the instances of this context
      --domain string    Only this instance
      --dry-run          Report what would be published without writing
      --force            Do not ask for confirmation before publishing
  -h, --help             help for personal-contacts
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

