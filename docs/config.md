[Table of contents](README.md#table-of-contents)

# Configuration

## Main Configuration file

You can configure your `cozy-stack` using a configuration file.

By default, `cozy-stack` comes with a file named [`cozy.example.yaml`](https://github.com/cozy/cozy-stack/blob/master/cozy.example.yaml).

If you need to edit the configuration, we recommend to only copy the
needed part in a new file. This new file should be named `cozy.yaml`,
`cozy.yml`, or `cozy.json` depending on the format of your chosing,
and should be present in one of these directories (ordered by priority):

-   `./.cozy`
-   `$HOME/.cozy`
-   `/etc/cozy`

The path of the configuration file can also be define from an absolute path
given by the `--config` (or `-c`) flag of the [cozy-stack command](./cli/cozy-stack_serve.md).

Note that is is possible to have an additional configuration file, with the
`.local` suffix. For example, it can be used to have `/etc/cozy/cozy.yml`
managed by a package manager, and `/etc/cozy/cozy.yml.local` for things that a
user can customize.

### Templating and Environment Variables

It is possible to pass environnment variable to this configuration using the
[template language of golang](https://golang.org/pkg/text/template/), delimited
by `{{` and `}}`.

The environment variables are available in the `.Env` variable. For instance the
text `{{.Env.COUCHDB_PASSPHRASE }}` will be replaced by the value of the
`COUCHDB_PASSPHRASE` environment variable. The template is evaluated at startup
of the stack.

### Values and Example

To see the detail of the available parameters available, you can see an example
of configuration in the [cozy.example.yaml](https://github.com/cozy/cozy-stack/blob/master/cozy.example.yaml)
file at the root of this repository.

This file contains all the parameters and fields that can be used to configure
the stack with some example values.

Some fields can be overriden by the flags of the
[cozy-stack serve command](cli/cozy-stack_serve.md).

## Stack endpoints

By default, `cozy-stack` use plain-text & local socket for client
(`localhost:8080`) and admin (`localhost:6060`) communications.

If you want to control a remote stack or using TLS to secure communications, you
can configure your `cozy-stack` client with the following CLI arguments or
environment variables.

| Argument                  | Env variable                                       | Default value | Usage |
|---------------------------|----------------------------------------------------|---------------|-------|
| `--host` / `--admin-host` | `COZY_HOST` / `COZY_ADMIN_HOST`                    | localhost     | `[http[s]://]<fqdn>[:<port>]` |
| `--port` / `--admin-port` | `COZY_PORT` / `COZY_ADMIN_PORT`                    | 8080 / 6060   |  |
|                           | `COZY_HOST_TIMEOUT` / `COZY_ADMIN_TIMOUT`          | 15s           | HTTP timeout to use<br />Must be [a valid golang duration](https://golang.org/pkg/time/#ParseDuration) like `10s` or `1m` |
|                           | `COZY_HOST_VALIDATE` / `COZY_ADMIN_VALIDATE`       | true          | Enable HTTPS certificate validation<br />Can also be set via host URL query part, like `https://localhost:6060?validate=false` |
|                           | `COZY_HOST_CA` / `COZY_ADMIN_CA`                   | none          | CA file to use for HTTPS certificate validation<br />Can also be set via host URL query part, like `https://localhost:6060?ca=<ca>` |
|                           | `COZY_HOST_CERT` / `COZY_ADMIN_CERT`               | none          | Client certificate to use<br />Can also be set via host URL query part, like `https://localhost:6060?cert=<cert>` |
|                           | `COZY_HOST_KEY` / `COZY_ADMIN_KEY`                 | none          | Client certificate to use<br />Can also be set via host URL query part, like `https://localhost:6060?key=<key>` |
|                           | `COZY_HOST_FINGERPRINT` / `COZY_ADMIN_FINGERPRINT` | none          | Hex-encoded SHA-256 key pinning to use<br />Can also be set via host URL query part, like `https://localhost:6060?fp=<fp>`<br /><br />You can get the fingerprint of a given certificate with<br />`openssl x509 -in <certificat.crt> -pubkey \| openssl pkey -pubin -outform der \| openssl dgst -sha256 -hex`<br />or directly from a private key with `openssl pkey -in <key.pem> -pubout -outform der \| openssl dgst -sha256 -hex` |

## Administration secret

To access to the administration API (the `/admin/*` routes), a secret passphrase
should be stored in a `cozy-admin-passphrase`. This file should be in one of the
configuration directories, along with the main config file.

The passphrase is stored in a salted-hashed representation using scrypt. To
generate this file, you can use the `cozy-stack config passwd [filepath]`
command. This command will ask you for a passphrase and will create the
`cozy-admin-passphrase` at the specified path.

You can use the `COZY_ADMIN_PASSPHRASE` (or `COZY_ADMIN_PASSWORD`) env variable
if you do not want to type the passphrase each time you call `cozy-stack`.

### Example

```sh
$ mkdir ~/.cozy && cozy-stack config passwd ~/.cozy/cozy-admin-passphrase
Hashed passphrase will be writtent in ~/.cozy/cozy-admin-passphrase
Passphrase:
Confirmation:
$ cat ~/.cozy/cozy-admin-passphrase
scrypt$16384$8$1$936bd62faf633b5f946f653c21161a9b$4e0d11dfa5fc1676ed329938b11a6584d30e603e0d06b8a63a99e8cec392d682
```

## Temporary files

The stack can use some temporary directories and files (execution of Image
Magick, konnectors and services for example). And they can take several GB for
the case of importing a Cozy. If needed, it is possible to configure the
directory where they will be created via the `TMPDIR` environment variable.

## Storage backend migration

The `fs.url` parameter configures the storage backend (`file://`,
`swift://` or `s3://`) used by all instances, as shown in
[cozy.example.yaml](https://github.com/cozy/cozy-stack/blob/master/cozy.example.yaml)
and detailed in the [S3 storage backend](s3.md) documentation.

To move instances to a different backend one at a time, without changing
the backend used by the rest of the fleet, an optional `fs.migration_target`
key can be set to a second storage URL. Its connection is initialized
alongside the default one at startup, so instances can be migrated to it
while `fs.url` keeps pointing at the previous backend:

```yaml
fs:
  url: swift://openstack/?UserName={{ .Env.OS_USERNAME }}&Password={{ .Env.OS_PASSWORD }}
  migration_target: s3://s3.rbx.io.cloud.ovh.net?access_key=ACCESS&secret_key=SECRET&region=rbx&bucket_prefix=cozy&use_ssl=true
```

As with `fs.url`, S3 credentials are passed as `access_key` and `secret_key`
query parameters of the URL.

Once `fs.migration_target` is set, the
[`cozy-stack instances migrate-storage`](cli/cozy-stack_instances_migrate-storage.md)
command can move individual instances to it. See
[Migrating an instance from Swift to S3](s3.md#migrating-an-instance-from-swift-to-s3)
for the full procedure, including rollback.

## Multiple CouchDB clusters

With a large number of instances, a single CouchDB cluster may not be enough.
Most cozy-stack administrators can ignore this section, but if you need this
advanced settings, let's see how to configure it. We start with a classical
configuration for a single cluster:

```yaml
couchdb:
  url: http://cluster1:5984/
```

The first step is to add the new cluster, but still forces the creation on the
first cluster for the moment:

```yaml
couchdb:
  url: http://cluster1:5984/
  clusters:
    - url: http://cluster1:5984/
      instance_creation: true
    - url: http://cluster2:5984/
      instance_creation: false
```

Then, we can restart the cozy-stack and everything will work the same, except
that the stack now knows how to use an instance on the new cluster. We can now
use the second cluster for instance creations instead of the first:

```yaml
couchdb:
  url: http://cluster1:5984/
  clusters:
    - url: http://cluster1:5984/
      instance_creation: false
    - url: http://cluster2:5984/
      instance_creation: true
```

**Notes:**

1. if several CouchDB clusters are configured to accept creations, a new
   instance will be put in a CouchDB cluster taken randomly

2. each instance document will keep the list index of the CouchDB cluster used
   for its databases, so don't remove a cluster in the middle of the list!

## OnlyOffice

An integration between Cozy and OnlyOffice has been made. It allows the
collaborative edition of office documents in the browser. There are some
pre-requistes. OnlyOffice Docs must be installed on a server ([The community
edition](https://helpcenter.onlyoffice.com/installation.aspx) is free but
limited to 20 connections and doesn't have the mobile mode). It can be the
same server as the stack, but it is tricky as the default port of the stack
and of the spellchecker of OnlyOffice is 8080 for both. The stack and the
OnlyOffice components must be able to make HTTP requests to each other.
And, for security, these requests can be signed via some shared secrets,
called inbox and outbox secrets in OnlyOffice configuration.

In the `local.json` configuration file of OnlyOffice, we need:

1. `services.CoAuthoring.token.enable.browser`: `true`
2. `services.CoAuthoring.token.enable.request.inbox`: `true`
3. `services.CoAuthoring.token.enable.request.outbox`: `true`
4. `services.CoAuthoring.secret.inbox`: a random secret
5. `services.CoAuthoring.secret.outbox`: another random secret

In the cozy configuration file, we need to add a section with:

```yaml
office:
  default:
    onlyoffice_url: https://onlyoffice.example.net/
    onlyoffice_inbox_secret: inbox_secret
    onlyoffice_outbox_secret: outbox_secret
```

Don't forget to restart all the services after having made the changes to the
configuration files. And you should be able to edit office documents in your
browser via the Drive application.

## Customizing a context

### Intro

In the config file of cozy-stack, it's possible to declare some contexts, that
are a way to regroup some cozy instances to give similar configuration. For
example, it is possible to give a `default_redirection` that will be used
when the user logs into their cozy. You can find more example in the example
config file.

### Assets

The visual appearance of a cozy instance can be customized via some assets
(CSS, JS, images). These assets can be inserted from the command-line with the
[`cozy-stack assets add`](./cli/cozy-stack_assets_add.md)
command.

Here are a small list of assets that you may want to customize:

- `/styles/theme.css`: a CSS file where you can override the colors and put
  other CSS rules
- `/favicon.ico`: the old-school favicon
- `/icon.svg`: the favicon in SVG format
- `/icon-192.png` and `/icon-512.png`: the two variants of the
  favicon
- `/apple-touch-icon.png`: the same but for Apple
- `/images/default-avatar.png`: the image to use as the default avatar.
- `/images/default-wallpaper.jpg`: the image to use as the default wallpapper
  on the home.
- `/images/icon-cozy-home.svg`: the home icon used and displayed by the cozy-bar.
- `/images/icon-cozy-home-inverted.svg`: the home icon used and displayed by the cozy-bar when using the theme inverted.
