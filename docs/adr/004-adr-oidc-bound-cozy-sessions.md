# ADR: OIDC-bound Cozy sessions and access tokens

## Status

Draft

## Context

Cozy-stack can log users in with OIDC. After that login, the stack creates its
own local credentials: usually a Cozy session cookie or Cozy OAuth tokens.

Today those local credentials are not strongly linked to the upstream OIDC
session. The OIDC provider can expire or close the upstream login while Cozy
still has a valid local session.

This is confusing for apps running in iframes or WebViews. From Cozy's point of
view the user can still look logged in, while an app that depends on OIDC may be
expired or unable to restart the OIDC login inside the iframe.

We also have flows where the caller already has an OIDC access token and wants
to call stack endpoints such as `/intents`. Most stack endpoints do not
understand OIDC tokens directly. They expect a Cozy session, a webapp token, or
a Cozy OAuth token.

## Problem

We need one shared auth model for OIDC-backed callers.

The model should answer:

- How is a Cozy session linked to the upstream OIDC login?
- When should a Cozy session stop being valid if the OIDC login ends?
- Can an OIDC access token call normal stack endpoints?
- How do we map an OIDC token to existing Cozy permissions?
- How do we avoid one-off auth logic in endpoints like `/intents`?

## Goals

- Bind Cozy sessions to the upstream OIDC session when the provider gives us a
  `sid`.
- Let configured OIDC access tokens authenticate stack API requests through the
  normal permission middleware.
- Keep endpoint handlers unaware of whether the caller used Cozy OAuth, a
  webapp token, or OIDC.
- Reuse the existing `permission.Permission` model.
- Support `/intents` without adding a special auth path only for intents.
- Handle OIDC logout by invalidating only Cozy sessions and clients bound to
  the matching `sid`.
- Fail when an OIDC token cannot be validated.

## Non-goals

- Replacing Cozy OAuth tokens everywhere.
- Storing OIDC refresh tokens by default.
- Adding one auth implementation per endpoint.
- Changing the `session_code` primitive.
- Supporting providers that do not give enough information to validate tokens or
  sessions safely.

## Proposed Direction

Add an explicit OIDC binding object to local Cozy credentials created from OIDC.

For a Cozy session, the binding says: "this local session came from this
upstream OIDC login."

For a Cozy OAuth client or token, the binding says: "this local credential came
from this upstream OIDC login."

A binding should contain the useful stable identifiers we can validate later:

```text
OIDC binding
- context name
- issuer
- subject
- sid, when available
- token expiry, when relevant
- auth time, when available
```

The most important value is `sid`. It identifies the upstream OIDC login
session. If we have it, we should use it to bind and revoke Cozy sessions
precisely.

## Session Lifetime

A Cozy session should be tied to the upstream OIDC session when possible, not
blindly to the expiry of the OIDC token used during login.

Simple rule:

- If we have an OIDC `sid`, bind the Cozy session to that `sid`.
- If the provider later sends a logout event for that `sid`, invalidate only the
  matching Cozy sessions and clients.
- If the request uses an OIDC access token directly as bearer auth, the request
  is valid only while that access token is valid.
- If we do not have `sid` or another session signal, token expiry can be used as
  a conservative upper bound.

Why not always use token expiry for the Cozy session?

`id_token exp` usually means: "this signed login proof expires at this time." It
does not always mean: "the user's SSO session ends at this time."

`access_token exp` usually means: "this bearer token cannot be used after this
time." It is important for API requests, but it can be too short for a browser
session if the upstream SSO session is still alive.

So token expiry is useful, but it should not be the main session model when
`sid` is available.

## OIDC Access Token Authentication

The permission middleware should be able to accept a verified OIDC access token
when the instance context enables it.

The middleware should:

- verify signature, issuer, audience, expiry, and required claims;
- find the target Cozy instance;
- map the OIDC token to a Cozy permission document;
- attach the OIDC binding to the request context;
- reject the request when validation fails.

After that, endpoint handlers should keep using the normal permission checks.
They should not need to know that the request used OIDC.

For example, `/intents` should work because the OIDC token maps to a caller
permission, not because `/intents` has its own OIDC-specific shortcut.

The stack should not refresh OIDC access tokens in this first version. If an
OIDC access token is expired, the request should fail and the caller should get
a fresh token from the OIDC side before retrying.

## Converting An OIDC Token To A Cozy Permission

The conversion should happen in the permission middleware, not in each endpoint.

Request-time flow:

- read the bearer token from the request;
- if normal Cozy token parsing succeeds, keep the existing behavior;
- if the token is an OIDC access token and the context allows it, validate it
  with the OIDC provider config for the target instance;
- verify signature, issuer, audience, expiry, and the claim that links the token
  to the target instance;
- build an OIDC binding from the verified claims: context, issuer, subject,
  `sid` when present, and token expiry;
- map the verified audience and scopes to a Cozy permission rule set;
- store the resulting `permission.Permission` in the request context, like
  existing Cozy token auth does today.

Unverified claims can be used only to choose a possible validation path. They
must not grant permissions.

For app tokens, the mapping can reuse the linked-app idea:

- configured token audience points to a `software_id`;
- `software_id` points to a linked app such as `registry://mail`;
- cozy-stack checks that this app is installed locally;
- the permission source becomes the app source, for example
  `io.cozy.apps/mail`;
- the permission rules come from the installed app permission document or
  manifest.

For non-app tokens, the mapping needs explicit configuration from OIDC scopes or
audiences to Cozy permission rules. There should be no implicit "all
permissions" mapping.

## Latency And Caches

The hot path should not depend on the OIDC provider network.

For JWT access tokens, a warm request should be mostly local work: verify the
JWT with cached keys, check claims, then load or build the Cozy permission. This
should be close to the existing OAuth-token path, which already verifies a JWT
and loads the OAuth client or permission data from CouchDB.

Cold requests are slower. Today cozy-stack already caches OIDC discovery
metadata and JWKS in `CacheStorage` for 24 hours. Without that cache, the stack
may need one or two HTTPS calls to the provider. Those calls can take hundreds
of milliseconds and have second-level timeouts, so they must not happen on every
API request.

Existing caches we can reuse:

- `CacheStorage`: generic Redis or in-memory cache with TTL support;
- OIDC discovery cache: `oidc-config:<well-known-url>`, currently 24 hours;
- OIDC JWKS cache: `oidc-jwk:<jwks-url>`, currently 24 hours;
- request-local permission cache: `GetPermission` stores the computed
  `permission.Permission` in the Echo context;
- OIDC `sid` binding store in `SessionStorage`, used to find local sessions and
  OAuth clients during logout.

Caches we should add:

- a short-lived token-to-permission cache keyed by a hash of the OIDC access
  token, never by the raw token value;
- cache TTL should be the minimum of token expiry and a small configured maximum
  such as a few minutes;
- the cached value should include the Cozy permission and the OIDC binding used
  to create it;
- if we enforce logout for bearer tokens with `sid`, add a revoked-`sid` marker
  or an index from `sid` to cached token keys, so logout cannot leave cached
  bearer permissions alive;
- refresh JWKS once when a token has an unknown `kid`, because providers rotate
  keys;
- use request coalescing around discovery/JWKS refreshes to avoid many parallel
  requests when the cache is cold.

If the provider uses opaque access tokens, validation needs introspection. That
is a provider network call, so opaque-token support should require a separate
short introspection cache. Its TTL must also be capped by the token expiry.

A bound Cozy session can make this faster after the first successful auth step.
When the browser or WebView can use the Cozy session cookie, the stack can use
the normal Cozy app bootstrap path and local Cozy credentials. Later API calls
do not need to validate the OIDC access token every time. This is an
optimization and a consistency feature, not a replacement for bearer-token auth,
because iframe and WebView cookie behavior is not always reliable.

## Permission Mapping

Permission mapping still needs a final design.

Possible options:

- Reuse the existing linked-app or `app_token_exchange` mapping for app tokens.
- Add a separate OIDC access-token mapping for non-app callers.
- Allow only specific audiences and scopes to become Cozy permissions.

The important rule is that an OIDC token must never become a broad Cozy
permission implicitly. The mapping must be configured and explicit.

## Configuration

This should be opt-in per OIDC context.

Possible shape:

```yaml
authentication:
  context-name:
    oidc:
      bind_sessions: true
      allow_access_token_authentication: true
```

Names are placeholders. The final config should keep two features separate:

- `bind_sessions` links Cozy sessions to the upstream OIDC session and makes
  logout/lifetime checks use that binding;
- `allow_access_token_authentication` allows verified OIDC access tokens to be
  used as bearer auth for stack API requests.

## Migration

Existing Cozy sessions and OAuth clients will not have an OIDC binding.

Session binding should apply to sessions and local credentials created after the
feature is enabled.

Existing unbound sessions should keep their current behavior until normal
expiration. We cannot safely recreate their upstream OIDC `sid` after the fact,
so binding them retroactively would be guesswork.

If an operator needs a hard cutover, they can invalidate old sessions as a
separate deployment action.

## Security Notes

- Never trust OIDC claims before token validation succeeds.
- OIDC access-token auth must still go through normal Cozy permission checks.
- Do not store raw OIDC access or refresh tokens unless another ADR explicitly
  decides it.
- Back-channel logout should use `sid` when available, not log out every session
  for an instance.
- If a provider does not give `sid`, token expiry can be used as a safer upper
  bound, but this should be called out in config and docs.

## Open Questions

- Is `app_token_exchange` enough for OIDC access-token permission mapping, or do
  we need a new config block?
- Where exactly do we store bindings for OAuth flows: OAuth client, token
  metadata, session, or more than one?
- Do we need token introspection for providers whose access tokens are opaque?
- What should happen when the provider has no `sid`?

## Decision

TBD.

## Consequences

TBD.
