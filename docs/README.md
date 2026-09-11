---
title: Cozy-Stack documentation - Table of contents
---

## What is the Cozy-Stack?

The cozy-stack is the main backend server for the Cozy platform.

[Full Cozy-Stack documentation here](https://docs.cozy.io/en/cozy-stack/).

The Cozy-Stack is in charge of serving / running the applications users have installed on their Cozy.

It is in charge of:

 - creating, updating, deleting documents inside the database;
 - authenticating users and client applications;
 - sending emails;
 - launching jobs on the server. Connectors that import data from remote websites are jobs. Jobs can be one time tasks (sending a message) or periodic tasks. Jobs that require executing third party code on the server side (like connectors), are sandboxed;
 - database replication API, allowing to sync documents between the server and local databases, for example in mobile clients.

Feel free to [open an issue](https://github.com/cozy/cozy-stack/issues/new) for questions and suggestions.


## How-to guides

### Usage

-   [Install the cozy-stack](INSTALL.md)
-   [Configuration file](config.md)
-   [Managing Instances](instance.md)
-   [Security](security.md)
-   [Manpages of the command-line tool](cli/cozy-stack.md)
-   [Using the admin API](admin.md)
-   [Important changes](important-changes.md)

### For developers

-   [Using the HTTP API of cozy-stack](http-api.md)
-   [Develop a client-side app](client-app-dev.md)
-   [Develop a konnector](konnectors-dev.md)
-   [Running and building Docker images](docker.md)
-   [Adding a new doctype](doctype.md)
-   [Working with the stack assets](assets.md)
-   [Build a release](release.md)
-   [The contributing guide](CONTRIBUTING.md)

## Explanation

### Up-to-date

-   [Banners](banners.md)
-   [Flagship app](flagship.md)
-   [Move design](move-design.md)
-   [Realtime internals](realtime-internals.md)
-   [Sharing design](sharing-design.md)
-   [Workflow of the konnectors](konnectors-workflow.md)

### Archives

These pages are the results of studies we made. They may be outdated and are
kept as an archive to help understanding what were out original intentions when
designing new services.

-   [General overview of the initial architecture](archives/architecture.md)
-   [Onboarding with an application](archives/onboarding.md)
-   [Moving](archives/moving.md)
-   [Golang Couchdb Plugins](archives/couchdb-plugins.md)
-   [Konnectors design](archives/konnectors-design.md)
-   [Replication](archives/replication.md)
-   [Realtime design](archives/realtime.md)

## Reference

### List of services

-   `/ai` - [AI](ai.md)
-   `/auth` - [Authentication & OAuth](auth.md)
    -   [Delegated authentication](delegated-auth.md)
-   `/apps` - [Applications Management](apps.md)
    -   [Apps registry](registry.md)
    -   [Konnectors](konnectors.md)
-   `/bitwarden` - [Bitwarden](bitwarden.md)
-   `/connection_check` - [Connection check](connection-check.md)
-   `/contacts` - [Contacts](contacts.md)
-   `/data` - [Data System](data-system.md)
    -   [Mango](mango.md)
    -   [CouchDB Quirks](couchdb-quirks.md) &
        [PouchDB Quirks](pouchdb-quirks.md)
-   `/files` - [Virtual File System](files.md)
    -   [Not synchronized directories](not-synchronized-vfs.md)
    -   [References of documents in VFS](references-docs-in-vfs.md)
-   `/intents` - [Intents](intents.md)
-   `/jobs` - [Jobs](jobs.md)
    -   [Workers](workers.md)
-   `/move` - [Move, export and import an instance](move.md)
-   `/notes` - [Notes with collaborative edition](notes.md)
-   `/notifications` - [Notifications](notifications.md)
-   `/office` - [Collaborative edition of Office documents](office.md)
-   `/permissions` - [Permissions](permissions.md)
-   `/public` - [Public](public.md)
-   `/realtime` - [Realtime](realtime.md)
-   `/remote` - [Proxy for remote data/API](remote.md)
    -   [NextCloud](nextcloud.md)
-   `/settings` - [Settings](settings.md)
    -   [Terms of Services](user-action-required.md)
-   `/sharings` - [Sharing](sharing.md)
    -   [Shared drives](shared-drives.md)
-   `/shortcuts` - [Shortcuts](shortcuts.md)
-   `/.well-known` - [Well-known](wellknown.md)
