# Intent feature on mobile — auto-login WebView via session_code

**Date:** 2026-07-09
**Status:** Design — approved in brainstorming session
**Scope:** cozy-stack (Go)

## Context

The Intent feature lets a client-side app delegate an action (PICK, OPEN, EDIT,
…) to another installed webapp. On web, the service app is loaded inside an
iframe and relies on the browser-shared `cozysessid` cookie to be logged in.

On mobile, the caller app is a native app and the service app is loaded in a
WebView. The cookie is not shared between the native app and the WebView, so the
service app loads unauthenticated and the intent cannot complete.

The caller app can already exchange an OIDC `id_token` for cozy-stack tokens via
`POST /auth/token_exchange` (`exchange_type=app`), which returns an app-scoped
OAuth access token. That token can query the intents API, but it cannot by itself
log in a WebView — it is a bearer token, not a cookie.

## Goal

Let a mobile caller app open an intent service app in a WebView that is already
logged in, scoped to that service app, by reusing the existing `session_code`
primitive.

## Non-goals

- A token-less / cookie-less WebView (that is approach B, not selected).
- Binding the session cookie to a single intent ID (deliberately not done; see
  Security).
- Changes to the flagship flow, the intents API, or `ServeAppFile`.

## Approach

Reuse the flagship `session_code` flow end-to-end. The only new behavior is
allowing an app-scoped token minted by `token_exchange` to mint a session_code,
where today `canCreateSessionCode` only accepts a maximal/flagship token or
passphrase+2FA.

### Data flow

```
Mobile app                cozy-stack                WebView (service app)
----------  --------------  ----------------------   ----------------------
[token_exchange]  ──POST /auth/token_exchange──▶
                   ◀── app-scoped access_token ──
[create intent]    ──POST /intents  (Bearer app token)──▶
                   ◀── intent doc: id + service href ──
[mint session_code]──POST /auth/session_code (Bearer app token)──▶
                   ◀── {"session_code": "<code>"} ──
[open WebView]     ── load https://<serviceSlug>.<inst>/?intent=<id>&session_code=<code> ──▶
                                                    ServeAppFile:
                                                    1. CheckAndClearSessionCode → ok
                                                    2. SetCookieForNewSession(NormalRun)
                                                    3. redirect to ?intent=<id> (code stripped)
                                                    4. on reload: relax CSP frame-ancestors
                                                    5. inject BuildAppToken(serviceSlug, sessID)
                   ◀── index.html with cozyData.token = <app token> ──
                                                    cozy-client in WebView uses app token
                                                    for all API calls (scoped to serviceSlug)
```

The service-app scoping is achieved by the existing app-token layer:
`ServeAppFile` injects `inst.BuildAppToken(serviceSlug, sessID)` into the served
`index.html` (`web/apps/serve.go:332` → `buildServeParams` → `getServeToken`),
which is a JWT with `subject=serviceSlug` scoped to that app's manifest
permissions. The cookie is a full user session, but the page only ever holds a
service-app-scoped token, and `GET /intents/:id` independently enforces
`intent.Services` on the bearer.

## Components to change

Four focused edits, no new files.

### 1. `web/auth/flagship.go` — `canCreateSessionCode` (line 85)

Add a new branch returning `allowedToCreateSessionCode` when the request is
authenticated with an app-scoped token from `token_exchange`. This is the only
behavioral change.

```go
func canCreateSessionCode(c echo.Context, inst *instance.Instance) canCreateSessionCodeResult {
    if err := middlewares.AllowMaximal(c); err == nil {
        return allowedToCreateSessionCode
    }
    if isAppTokenExchangeToken(c, inst) {                 // NEW
        return allowedToCreateSessionCode                 // NEW
    }

    // …existing passphrase + 2FA path unchanged…
}
```

### 2. `web/auth/flagship.go` — new helper `isAppTokenExchangeToken`

Lives next to `canCreateSessionCode` (locality with the only caller). Verifies
that the bearer token was minted by `token_exchange` for a linked app configured
on the instance's context.

Detection has two candidate implementations, to be settled during implementation:

- **(a) Custom claim** — add a marker claim (e.g. `"app_token_exchange": true`
  or a dedicated audience) to the access_token JWT minted by
  `buildTokenExchangeResponse` (`web/auth/token_exchange.go:419`). `isAppToken-
  ExchangeToken` reads the claim from the decoded JWT available on the echo
  context. Lightest at runtime, but touches the token_exchange output shape
  (additive, non-breaking).

- **(b) OAuth client lookup** — read the `client_id` claim from the decoded
  access token, fetch the `oauth.Client` via `oauth.FindClient`, and check that
  its `SoftwareID` matches one of the entries in
  `config.GetOIDCAppTokenExchange(inst.ContextName).Apps` (which currently has
  `Audience`, `SoftwareID`, `InstanceClaim` per app). Also verifies the linked
  app is installed on the instance, mirroring `tokenExchangeCheckAppInstance`
  (`web/auth/token_exchange.go:324`). No change to the JWT, but adds a CouchDB
  read per mint (acceptable: session_code minting is rare).

Either way, the predicate must confirm the token's linked app is currently
configured and installed, so a token whose config has been revoked cannot mint
codes.

### 3. Tests — `web/auth/flagship_test.go` (or colocated `flagship_app_token_test.go`)

Three cases, following the existing `web/auth/auth_test.go` token_exchange test
scaffolding (which already sets up `allow_app_token_exchange` + `app_token_ex-
change` config and a linked app — see `auth_test.go:2152`):

1. **App-scoped token mints a session_code** — mint an app token via the existing
   token_exchange test helper; `POST /auth/session_code` with that bearer; assert
   201 + `session_code` present; assert the code round-trips through the store.
2. **Non-app-scoped token still gated** — call `POST /auth/session_code` with a
   regular app token not from token_exchange (or no token); assert the response
   still requires passphrase + 2FA (no regression of the `need2FAToCreateSession-
   Code` path).
3. **Round-trip through ServeAppFile** — take the session_code from case 1, hit
   `https://<serviceApp>.<instance>/?intent=<id>&session_code=<code>`; assert
   redirect to `?intent=<id>` with `Set-Cookie: cozysessid=…`; assert a follow-up
   request with that cookie serves the app with `cozyData.token` populated by
   `BuildAppToken(serviceSlug, sessID)`. Reuses the
   `web/apps/apps_test.go:407-424` scaffolding pattern.

### 4. Docs — `docs/intents.md` (or wherever the intent mobile flow is documented)

Add a section describing the mobile + WebView flow: token_exchange → create
intent → session_code → load `https://<serviceApp>.<inst>/?intent=<id>&session_
code=<code>`.

## What is NOT changed

- `model/instance/auth.go:CreateSessionCode` — unchanged.
- `model/instance/store.go` session_code store — unchanged (single-use,
  7-day TTL, atomic delete on read).
- `web/apps/serve.go:167-183` — `ServeAppFile` session_code consumer unchanged
  (uses `session.NormalRun`, strips the code, redirects).
- `web/intents/intents.go` — create/get handlers unchanged.
- `model/intent/intents.go:GenerateHref` — unchanged.
- Flagship flow, magic link, delegated JWT — unchanged.

## Config & gating

No new config flag. The ability to mint a session_code is granted implicitly to
any token that satisfies `isAppTokenExchangeToken`, i.e. any token minted by
`token_exchange` for a linked app currently listed in the context's
`app_token_exchange` config. Revoking an app from `app_token_exchange` revokes
its ability to mint session_codes going forward.

If a deployer later wants to separate "can exchange id_token" from "can mint
session_code" per app, add an optional `allow_session_code` field (default
`true`) under each `OIDCAppTokenExchangeAppConfig` in
`pkg/config/config/config.go:439`. Deferred to YAGNI until a real need surfaces.

## Session duration

`session.NormalRun` (browser-session, 30-day max age cap — `model/session/ses-
sion.go:81`). Matches the existing `ServeAppFile` session_code consumer
(`web/apps/serve.go:172`) and the flagship flow, so no special-casing. Avoids
mid-intent expiry if the user is slow in a file picker, etc.

## Security considerations

The design reuses a battle-tested primitive, so the security surface is small
and mostly inherited.

- **Cookie scope** — `cozysessid` is a full user session on the instance domain
  (HttpOnly, Secure, SameSite=Lax, scoped to `.instance.tld`). Same risk
  profile as flagship. Bounded by: single-use session_code (atomic delete in
  Redis on read — `model/instance/store.go:88-98`), 7-day mint TTL, NormalRun
  session, WebView-only lifecycle.
- **WebView boundary** — the cookie lives in the WebView's cookie jar, not the
  mobile app's native storage. If the WebView is destroyed (standard iOS/Android
  behavior), the cookie is cleared. This is a mobile-side implementation
  responsibility, not a stack change.
- **Token elevation** — the app-scoped token gains the ability to mint a user
  session code. This is an elevation in effect, but the caller already proved
  OIDC identity to cozy-stack during `token_exchange` (`validateTokenExchange-
  AppToken`, `web/auth/token_exchange.go:297`), so minting a session_code is a
  natural follow-on.
- **Bound to a configured linked app** — `isAppTokenExchangeToken` verifies the
  token's linked app is in `app_token_exchange` and installed, mirroring
  `tokenExchangeCheckAppInstance`. Prevents a stray app token from minting codes.
- **Intent binding (deliberate non-feature)** — the session_code is NOT bound to
  the intent ID. The minted session is a generic user session; the intent is
  consumed via `?intent=` which independently enforces `intent.Services` on
  `GET /intents/:id`. Keeping them decoupled matches the flagship pattern (mint
  code → use it wherever) and avoids new coupling. Single-use session bound to
  one intent is approach B, not A.
- **No CSRF on mint** — `POST /auth/session_code` requires a valid bearer token
  (not a cookie-CSRF flow); the new branch inherits that.
- **Audit logging** — keep the existing login-entry logging in `canCreateSes-
  sionCode`'s session-creation path. Use a distinct `source` value
  (`"app_token_exchange"`) so audit logs distinguish app-token-minted codes from
  flagship (`"flagship"`) and password (`"password"`) ones.

## Testing strategy

See "Components to change" §3. Follow the existing `web/auth/*_test.go` style
(`github.com/stretchr/testify` + `github.com/cozy/check`, shared test instance
helper, CouchDB setup). No new test infrastructure.

## Alternatives considered

- **Approach B — Bearer token via `?sharecode=`-style injection (no cookie).**
  Mint a scoped JWT bound to the intent + service app, pass via `?sharecode=`,
  reuse `ServeAppFile`'s sharecode path. No cookie ever leaves the stack —
  strongest isolation. Rejected: more new code (new minting endpoint, new token
  shape, permission-check adjustments for `GET /intents/:id`), and the
  cookie-free page cannot use cozy-bar / cross-app navigation. Acceptable
  trade-off for a focused intent WebView, but A is cheaper and reuses more.
- **Approach C — Intent-bound scoped session.** Add a `BoundApp` field to the
  session doc and enforce it in `LoadSession`/`BuildAppToken`. Strongest cookie-
  level isolation. Rejected: touches the core session model with the largest
  blast radius; overkill given the app-token layer already scopes API calls in
  approach A.

## Open implementation notes

- Detection mechanism (custom claim vs OAuth client lookup) — see §2. Decide
  during implementation; (b) is preferred to avoid touching the token_exchange
  JWT output, unless performance profiling of session_code minting says
  otherwise (it won't — mints are rare).
- Exact placement of the audit `source` string — confirm against existing
  login-entry sources in `model/instance/lifecycle` or wherever login entries
  are recorded, to keep naming consistent.
