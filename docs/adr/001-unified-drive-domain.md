# ADR: Unified Drive Domain Architecture

## Status

**Proposed/Draft**

## Context

### Problem

When UserA shares a file URL copied from the browser address bar with UserB, the link
is bound to UserA's instance host and often fails for UserB.

1. UserA copies `https://usera-drive.twake.app/drive/#/folder/251f...`
2. UserA sends the URL to UserB
3. UserB opens the link on the wrong instance host and access fails or the route no
   longer matches their context

**Root cause:** the browser URL currently identifies a user's instance. The host is part
of the data context, so copying the URL does not provide a stable organization-level
entrypoint.

### Current Architecture

```text
User A: https://usera.twake.app/drive/
        -> instance A data

User B: https://userb.twake.app/drive/
        -> instance B data
```

### Constraints

- No dedicated "Copy Link" button: users copy the browser URL directly
- Keep the current hash-based Drive routing model where possible
- Minimize extra user interaction during access
- Cross-organization sharing is secondary priority

## Proposal

### Unified Drive Domain

Replace user-specific browser hosts with a single Drive host per organization while
keeping the current hash-based routing style.

```text
Current:  https://alice-drive.twake.app/#/folder/251f...
Proposed: https://drive.twake.app/#/folder/251f...
```

The main change is the host. The route format remains familiar for the frontend and for
users. `drive.<org-domain>` is the page host; after bootstrap, the resolved workplace
instance becomes the API and realtime host.

### Chosen Architecture: Browser-Direct Access

`drive.<org-domain>` serves the Drive application shell and handles auth/bootstrap.
After authentication, the browser resolves the user's workplace instance and talks
directly to that instance for API and realtime traffic.

High-level flow:

1. User opens `drive.<org-domain>`
2. Drive authenticates the user and resolves their workplace instance
3. The frontend initializes Drive against that instance
4. API and realtime requests go directly from the browser to the resolved instance
5. If a route needs extra context, such as shared-drive context, the frontend resolves
   it after bootstrap

### Scope

This ADR covers authenticated Drive access and Unified Drive public links inside one
organization/site. It does not yet define the final handling for cross-organization
sharing or the complete migration strategy from legacy URLs.

## Alternatives

### Alternative 1: Backend Routing Through cozy-stack

Use the Unified Drive host as the main API origin and route each request internally to
the correct instance inside cozy-stack.

- Pros: closer to the current single-origin frontend model
- Cons: keeps routing complexity in the backend hot path for every request and adds a
  backend routing/proxy layer

### Alternative 2: Keep Per-Instance Drive URLs

Keep the existing `user-drive...` hosts and improve only redirection or discovery.

- Pros: smallest short-term change
- Cons: does not solve the browser address-bar copy/paste problem

### Alternative 3: Full OAuth-Style Browser Access

Move to a more explicit browser auth model based on OAuth/OIDC tokens instead of
the current app bootstrap model.

- Pros: cleaner long-term browser auth model
- Cons: larger auth redesign than needed for the first iteration

## Decision

**Adopt the Unified Drive domain with browser-direct access to the target instance.**

## Consequences

### Positive

1. URL sharing becomes organization-centric instead of user-centric
2. The browser URL can stay stable on `drive.<org-domain>`
3. The frontend can keep the current hash-router approach
4. File APIs and realtime stay close to the owning workplace instance

### Negative

1. Frontend/bootstrap logic becomes more complex than in the current single-origin model
2. Token creation and refresh must become explicit instead of being tied to the target
   instance serving the app
3. Some routes still need extra context resolution after startup, especially around
   shared drives
4. Cross-organization sharing remains a follow-up topic

## Implementation

### Assumptions
* We will have one Unified Drive host per organization, for example `drive.acme.com`
* We have a clean way to resolve auth context configuration from `org_domain`

### User Journey: User Opens Drive

#### User opens `https://drive.acme.com/#/folder/...`
   - Add a Unified Drive host: new handler in `routing.go` and host recognition for
     `drive.<org-domain>`
   - Until Unified Drive session/bootstrap is complete, redirect to the auth/bootstrap
     flow instead of serving the authenticated Drive shell
   - After bootstrap, serve `index.html` and static Drive assets from the Unified Drive
     host
   - Asset bytes may still come from the resolved instance's Drive bundle


#### If the user is not authenticated, Drive starts the auth/bootstrap flow
   - Resolve `org_domain` -> `auth context`
   - If OIDC is configured for that context, use the OIDC flow below
   - If no OIDC is configured, use the discovery/login fallback below
   - After either flow completes, create a Unified Drive session on `drive.<org-domain>`

#### OIDC login flow
   - Start a context-first OIDC flow from Unified Drive
   - Unified Drive callback resolves the user's workplace instance
   - Unified Drive creates a session on `drive.<org-domain>`
   - Store `workplaceFQDN` and the originally requested Drive URL in that session
   - Redirect back to the requested Unified Drive URL

#### Login flow without OIDC
   - Show a discovery page on Unified Drive
   - User enters email, workplace URL, or slug
   - Resolve the real workplace instance
   - Redirect to that instance's existing `/auth/login`
   - After successful instance login, return to Unified Drive
   - Unified Drive creates its own session, stores `workplaceFQDN`, and redirects back to
     the requested Drive URL

#### After login, Unified Drive serves the Drive shell
   - Split page host from target backend host
   - Serve the page and static resources from `drive.<org-domain>`
   - Inject the minimal bootstrap contract:
     - resolved workplace instance host
     - token/bootstrap information for browser-direct calls to that instance
     - locale, flags, and capabilities from the resolved instance
     - a token refresh path or mechanism that does not rely on the current page origin

#### The frontend initializes Drive against the resolved instance
   - Stop assuming the current page origin is the API origin
   - Initialize `cozy-client` with the resolved workplace instance
   - Prepare realtime initialization against the same target instance

#### The frontend loads the requested route
   - Keep current route parsing in the browser
   - Add a lightweight route/context resolution step after shell load when extra sharing
     context is needed

#### The browser sends API and realtime requests directly to the resolved instance
   - Update token/bootstrap and refresh flows for direct instance access
   - Adjust CORS and related auth configuration so `drive.<org-domain>` can be the
     frontend origin

#### The user copies the URL from the browser
   - Ensure the browser always shows the Unified Drive host
   - Define migration and redirection from legacy per-instance Drive URLs

### User Journey: User Opens a Public Link

#### User opens a public Unified Drive URL
   - Public Unified Drive URLs must encode the target instance identity
   - The encoded instance identity must be visible to the server, not only inside the
     hash fragment
   - Unified Drive resolves the target instance before serving the public shell

#### Unified Drive serves the public Drive shell
   - Serve the public page and static assets from `drive.<org-domain>`
   - Reuse the existing public Drive frontend after the target instance is resolved
   - Keep the existing share-by-link permission model on the target instance

#### The public frontend initializes against the resolved instance
   - Bootstrap the public app against the resolved instance
   - Keep support for existing public-link protections such as password checks
   - Send public-link API requests directly from the browser to the resolved instance
