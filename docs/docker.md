[Table of contents](README.md#table-of-contents)

# Docker

This page list various operations that can be automated _via_ Docker when
developing cozy-stack.

For docker usage in production to self-host your cozy instance, please refer
to our [Self Hosting Documentation](https://docs.cozy.io/en/tutorials/selfhosting/).

## Running a CouchDB instance

This will run a new instance of CouchDB in `single` mode (no cluster). This
command exposes couchdb on the port `5984`.

```bash
$ docker run -d \
    --name cozy-stack-couch \
    -p 5984:5984 \
    -e COUCHDB_USER=admin -e COUCHDB_PASSWORD=password \
    -v $HOME/.cozy-stack-couch:/opt/couchdb/data \
    couchdb:3.3
$ curl -X PUT http://admin:password@127.0.0.1:5984/{_users,_replicator}
```

Verify your installation at: http://127.0.0.1:5984/_utils/#verifyinstall.

Note: for running some unit tests, you will need to use `--net=host` instead of
`-p 5984:5984` as we are using CouchDB replications and CouchDB will need to be
able to open a connexion to the stack.

## Building a cozy-stack _via_ Docker

Warning, this command will build a linux binary. Use
[`GOOS` and `GOARCH`](https://golang.org/doc/install/source#environment) to
adapt to your own system.

```bash
# From your cozy-stack developement folder
docker run -it --rm --name cozy-stack \
    --workdir /app \
    -v $(pwd):/app \
    -v $(pwd):/go/bin \
    golang:1.26 \
    go get -v github.com/cozy/cozy-stack
```

## Publishing a new cozy-app-dev image

We publish the cozy-app-dev image when we release a new version of the stack.
See `scripts/docker/cozy-app-dev/release.sh` for details.

## Docker run and url name for cozy-app-dev

A precision for the app name:

```bash
docker run --rm -it -p 8080:8080 -v "$(pwd)/build":/data/cozy-app/***my-app*** cozy/cozy-app-dev
```

***my-app*** will be the first part of: ***my-app***.cozy.localhost:8080

## Only-Office document server

### Shared secret

OnlyOffice and cozy-stack share a secret: it signs the configuration sent to
the editor (inbox) and the callbacks from OnlyOffice (outbox). cozy-stack
requires the outbox secret as soon as a document server URL is configured.
Generate the secret **once** and store it outside the repository:

```bash
(umask 077; printf 'JWT_SECRET=%s\n' "$(openssl rand -hex 32)" > "$HOME/.cozy/onlyoffice.env")
```

Reuse this file on subsequent restarts; do not generate a new secret unless
you also update the cozy-stack configuration below. Without a secret,
OnlyOffice generates a random one when the container is recreated, and
cozy-stack refuses its callbacks.

### Option 1: With Helper Script

Use the helper script that works on both Linux and macOS:

```bash
./scripts/start-oo.sh
```

The script automatically:
- Reads the secret from `~/.cozy/onlyoffice.env` and fails if it is missing
- Collects all instance hostnames from cozy-stack
- Starts OnlyOffice with JWT enabled and proper `--add-host` mappings
- Waits for OnlyOffice to be ready

### Option 2: Manual Docker Command

**For macOS and Linux (port mapping):**

```bash
docker run -d --name onlyoffice-ds \
    -p 8000:80 \
    --env-file "$HOME/.cozy/onlyoffice.env" \
    -e JWT_ENABLED=true \
    -e ALLOW_PRIVATE_IP_ADDRESS=true \
    -e ALLOW_META_IP_ADDRESS=true \
    --add-host=cozy.localhost:host-gateway \
    onlyoffice/documentserver:latest
```

**For Linux only (`--net=host`):**

```bash
docker run -d --name onlyoffice-ds \
    --net=host \
    --env-file "$HOME/.cozy/onlyoffice.env" \
    -e JWT_ENABLED=true \
    -e ALLOW_PRIVATE_IP_ADDRESS=true \
    -e ALLOW_META_IP_ADDRESS=true \
    -e DS_PORT=8000 \
    onlyoffice/documentserver:latest
```

### Configure cozy-stack

Add to your `~/.cozy/cozy.yaml`:

```yaml
contexts:
  default:
    onlyoffice_url: http://localhost:8000/

office:
  default:
    onlyoffice_url: http://localhost:8000/
    onlyoffice_inbox_secret: "<value of JWT_SECRET in ~/.cozy/onlyoffice.env>"
    onlyoffice_outbox_secret: "<value of JWT_SECRET in ~/.cozy/onlyoffice.env>"
```

Use the actual secret in the YAML, not `$JWT_SECRET`: the YAML value is not a
shell variable reference. Restart cozy-stack to load the configuration, then
reload the editor in the browser. Keep the default `Authorization` JWT header
so cozy-stack can verify callbacks. This setup was tested with Document Server
9.4.

### Check the host mapping

The helper script uses `go run . instances ls` to discover instance domains.
Running it outside the repository root can silently fall back to a
`cozy.localhost` mapping, which does not cover instances such as
`alice.localhost`. Verify the mapping for your actual instance:

```bash
docker exec onlyoffice-ds getent hosts alice.localhost
```

The instance must resolve to the host gateway. On Linux, also ensure cozy-stack
listens on an address reachable from Docker, for example with
`cozy-stack serve --host 0.0.0.0`. This exposes the development server beyond
loopback, so use it only on a trusted development network.

### Enable Feature Flag

```bash
cozy-stack features defaults '{"drive.office": {"enabled": true, "write": true}}'
```

### Troubleshooting

**OnlyOffice can't download the document:**
- Check OnlyOffice logs: `docker logs onlyoffice-ds`
- Look for DNS errors like `ENOTFOUND cozy.localhost`
- Ensure `--add-host` flag was applied: `docker exec onlyoffice-ds cat /etc/hosts | grep cozy`
- Ensure `ALLOW_PRIVATE_IP_ADDRESS=true` is set

**"Download failed" error in editor:**
- The Document Server can't reach cozy-stack
- Verify connectivity: `docker exec onlyoffice-ds wget -qO- http://cozy.localhost:8080/`

**No "Open with OnlyOffice" option in Drive:**
- Check feature flag: `cozy-stack features show`
- Verify context has `onlyoffice_url`: check `/settings/context` endpoint
