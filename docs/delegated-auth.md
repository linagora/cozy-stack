[Table of contents](README.md#table-of-contents)

# Delegated authentication

In general, the cozy stack manages the authentication itself. In some cases, an
integration with other softwares can be mandatory. It's possible to use JWT or
OpenID Connect, with a bit of configuration to do that.

## JWT

To enable an external system to create links with a JWT to log in users for
cozy instances in a given context, we just need to add the secret to use for
checking the JWT in the config, like this:

```yaml
authentication:
  the-context-name:
    jwt_secret: s3cr3t
```

The external system can then create a JWT, with the parameter `name` as the
instance domain, and send the user to `https://<instance>/?jwt=...`. The user
will be logged in, and redirected to its default application.

## Open ID Connect

OpenID Connect can also be used, and is more adapted when the users don't
always come from the authentication provider.

For OpenID Connect, there are more configuration parameters and they must be
configured per context. A context is set of configuration parameters and each
Cozy instance belongs to one context.

```yaml
authentication:
  the-context-name:
    disable_password_authentication: false
    oidc:
      client_id: aClientID
      client_secret: s3cret3
      scope: openid profile
      login_domain: login.mycozy.cloud
      redirect_uri: https://oauthcallback.mycozy.cloud/oidc/redirect
      authorize_url: https://identity-provider/path/to/authorize
      token_url: https://identity-provider/path/to/token
      userinfo_url: https://identity-provider/path/to/userinfo
      userinfo_instance_field: cozy_number
      userinfo_instance_prefix: name
      userinfo_instance_suffix: .mycozy.cloud
      allow_custom_instance: false
      allow_oauth_token: false
      id_token_jwk_url: https://identity-provider/path/to/jwk
      allow_app_token_exchange: true
      app_token_exchange:
        twake-mail-web:
          software_id: registry://mail
          instance_claim: workplaceFqdn
```

Let's see what it means:

- `disable_password_authentication` can be set to `true` to disable the classic
  password authentication on the Cozy, and forces the user to login with OpenID
  Connect.

And in the `oidc` section, we have:

- `client_id` and `client_secret` are the OAuth client that will be used to
  talk to the identity provider
- `scope` is the OAuth scope parameter (it is often `openid profile`)
- `login_domain` is a domain that is not tied to an instance, but allows to
  login with OIDC with the provider configured on this context
- `redirect_uri` is where the user will be redirected by the identity provider
  after login (it must often be declared when creating the OAuth client, and we
  have to use a static hostname, not the hostname of a cozy instance)
- `logout_url` can be set to redirect the user to this URL after they have been
  logged out
- `token_url`, `authorize_url`, and `userinfo_url` are the URLs used to talk to
  the identity provider (they ofter can be found by the discovery mechanism of
  OpenID Connect with the names `token_endpoint`, `authorization_endpoint`, and
  `userinfo_endpoint`)
- `userinfo_instance_field` is the JSON field to use in the UserInfo response
  to know the cozy instance of the logged in user.
- `userinfo_instance_prefix` and `userinfo_instance_suffix` are optional, and
  will be put before and after the field fetched from the UserInfo response to
  give the complete instance URL
- `allow_custom_instance` can be set to true to let the user chooses their
  instance name
- `allow_oauth_token` must be set to true to enable the
  `POST /oidc/access_token` route (see below for more details).
- `allow_app_token_exchange` can be set to true to enable user app exchanges
  on `POST /auth/token_exchange`. The request body must set
  `exchange_type` to `app` for this exchange.
- `app_token_exchange` maps an OIDC token audience to a trusted Cozy linked
  app. The `software_id` must use the `registry://<slug>` format. The stack
  derives the Cozy OAuth scope from the linked app manifest, so the OIDC
  provider does not need to expose `registry://...` as an OIDC scope. When
  `instance_claim` is set, the claim must contain the target Cozy instance
  domain and is used instead of the default OIDC instance mapping.
- Token exchange requires a `sid` claim so the created OAuth client can be
  revoked by OIDC backchannel logout.

With the example config, if the UserInfo response contains `"cozy_number":
"00001"`, the user can login on the instance `name00001.mycozy.cloud`.

When `allow_custom_instance` is set to true, the stack will look at the `sub`
field in the UserInfo response, and checks that it matches the `oidc_id` set
on this instance (and the `userinfo_instance_*` and `login_domain` fields are
ignored). If `id_token_jwk_url` is set, the client can send the ID token from
the provider instead of the access token. This token will be checked with the
key fetched from this URL, and the `sub` field of it must match the `oidc_id`
set in the instance.

### Routes

Let's see the 3 routes used in this process

#### GET /oidc/start

To start the OpenID Connect dance, the user can go to this URL. It will
redirect him/her to the identity provider with the rights parameter. The user
will also be redirected here if they are not connected and that the password
authentication is disabled.

```http
GET /oidc/start HTTP/1.1
Host: name00001.mycozy.cloud
```

```http
HTTP/1.1 303 See Other
Location: https://identity-provider/path/to/authorize?response_type=code&state=9f6873dfce7d&scope=openid+profile&client_id=aClientID&nonce=94246498&redirect_uri=https://oauthcallback.mycozy.cloud/oidc/redirect
```

#### GET /oidc/redirect

Then, the user can log in on the identity provider, and then he/she will be
redirected to this URL. Note that the URL is on a generic domain: the stack
will redirect the user to his/her instance (where it's possible to create
cookies to log in the user).

```http
GET /oidc/redirect?state=9f6873dfce7d&code=ccd0032a HTTP/1.1
Host: oauthcallback.mycozy.cloud
```

```http
HTTP/1.1 303 See Other
Location: https://name00001.mycozy.cloud/oidc/login?state=9f6873dfce7d&code=ccd0032a
```

#### GET /oidc/login

On this route, the stack can create the session for the user, with the cookies.

```http
GET /oidc/login?code=ccd0032a HTTP/1.1
Host: name00001.mycozy.cloud
```

```http
HTTP/1.1 303 See Other
Set-Cookie: ...
Location: https://name00001-home.mycozy.cloud/
```

If the `allow_oauth_token` option is enabled, it's possible to use an
access_token instead of code on this URL.

If the `id_token_jwk_url` option is enabled, it's possible to use an
id_token instead.

#### POST /oidc/twofactor

If the instance is protected with two-factor authentication, the login
route will render an HTML page with JavaScript to check if the user has trusted
the device. And the JavaScript submit a form to this route. If the trusted
device token is set, a session will be created. Else, a mail with a code is
sent, and the user is redirected to a page where they can type the two-factor
code.

```http
POST /oidc/twofactor HTTP/1.1
Host: name00001.mycozy.cloud
Content-Type: application/x-www-form-urlencoded

trusted-device-token=xxx&access-token=yyy&redirect=&confirm=
```

```http
HTTP/1.1 303 See Other
Set-Cookie: ...
Location: https://name00001-home.mycozy.cloud/
```

#### GET /oidc/bitwarden/:context

This route can be used by a bitwarden client to get a token from the OpenID
Connect Identity Provider, and the fqdn of the associated cozy instance. This
token can then be exchanged for credentials for the cozy instance.

```http
GET /oidc/bitwarden/examplecontext?redirect_uri=cozypass://login HTTP/1.1
Host: oauthcallback.mycozy.cloud
```

```http
HTTP/1.1 303 See Other
Location: https://identity-provider/path/to/authorize?response_type=code&state=9f6873dfce7d&scope=openid+profile+email&client_id=aClientID&nonce=94246498&redirect_uri=https://oauthcallback.mycozy.cloud/oidc/redirect
```

[...]

```http
HTTP/1.1 303 See Other
Location: cozypass://login?code=xxx&instance=alice.example.com
```

#### POST /oidc/bitwarden/:context

This route can be used by a bitwarden client to exchange the token from the
previous route for credentials.

```http
POST /oidc/bitwarden/examplecontext HTTP/1.1
Host: alice.example.com
Content-Type: application/x-www-form-urlencoded
```

```
code=xxx&
password=myHashedPassword&
client_id=mobile&
deviceType=0&
deviceIdentifier=aac2e34a-44db-42ab-a733-5322dd582c3d&
deviceName=android&
clientName=CozyPass&
devicePushToken=
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "client_id": "f05671e159450b44d5c78cebbd0260b5",
  "registration_access_token": "J9l-ZhwP[...omitted for brevity...]",
  "access_token": "eyJhbGciOiJSUzI1NiIsImtpZCI6IkJDMz[...](JWT string)",
  "expires_in": 3600,
  "token_type": "Bearer",
  "refresh_token": "28fb1911ef6db24025ce1bae5aa940e117eb09dfe609b425b69bff73d73c03bf",
  "Key": "0.uRcMe+Mc2nmOet4yWx9BwA==|PGQhpYUlTUq/vBEDj1KOHVMlTIH1eecMl0j80+Zu0VRVfFa7X/MWKdVM6OM/NfSZicFEwaLWqpyBlOrBXhR+trkX/dPRnfwJD2B93hnLNGQ=",
  "PrivateKey": null
}
```

#### POST /oidc/:context/logout

This route implements the OpenID Connect Back-Channel Logout. It means that the
SSO can call this endpoint to logout the user.

```http
POST /oidc/a-context/logout HTTP/1.1
Host: name00001.mycozy.cloud
Content-Type: application/x-www-form-urlencoded

logout_token=eyJhbGci ... .eyJpc3Mi ... .T3BlbklE ...
```

```http
HTTP/1.1 200 OK
Cache-Control: no-store
```

#### POST /oidc/access_token

This additional route can be used by an OAuth client (like a mobile app) when
delegated authentication via OpenID Connect is enabled. It allows the client to
obtain an `access_token` for requesting the cozy-stack in exchange of a token
valid on the OpenID Connect Identity Provider.

```http
POST /oidc/access_token HTTP/1.1
Host: name00001.mycozy.cloud
Accept: application/json
Content-Type: application/json
```

```json
{
  "client_id": "55eda056e85468fdfe2c8440d4009cbe",
  "client_secret": "DttCGIUOTniVNkivR_CsZ_xRoME9gghN",
  "scope": "io.cozy.files io.cozy.photos.albums",
  "oidc_token": "769fa760-59de-11e9-a167-9bab3784e3e7"
}
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "access_token": "ooch1Yei",
  "token_type": "bearer",
  "refresh_token": "ui0Ohch8",
  "scope": "io.cozy.files io.cozy.photos.albums"
}
```

If `id_token_jwk_url` option is set, the client can send an `id_token` instead
of an `oidc_token` in the payload.

If the flagship makes the request, it also can use a delegated code obtained
from the cloudery, by using `code` instead of `oidc_token`. When using a
delegated code, the session ID (`sid`) is already stored with the code (the
cloudery provides the `id_token` when requesting the delegated code), so the
flagship app does not need to send an `id_token` separately.

Cozy keeps the session identifier on the OAuth client and, when the user signs
out of the flagship app (which deletes the client), the stack performs a
best-effort call to the OpenID provider's `end_session_endpoint` so the
upstream SSO session is closed too. This complements the back-channel logout
endpoint (`POST /oidc/:context/logout`) that the provider can call to terminate
Cozy sessions.

**Note:** if the OAuth client asks for a `*` scope and has not been certified
as the flagship app, this request will return:

```http
HTTP/1.1 202 Accepted
Content-Type: application/json
```

```json
{
  "session_code": "ZmY4ODI3NGMtOTY1Yy0xMWVjLThkMDgtMmI5M2"
}
```

The `session_code` can be put in the query string while opening the OAuth
authorize page. It will be used to open the session, and let the user type the
6-digits code they have received by mail to confirm that they want to use this
app as the flagship app.

##### Special case of 2FA

When 2FA is enabled on the instance, the stack will first respond with:

```http
HTTP/1.1 403 Forbidden
Content-Type: application/json
```

```json
{
  "error": "two factor needed",
  "two_factor_token": "123123123123"
}
```

and the client must ask the user to type its 6-digits code, and then make again
the request:

```json
{
  "access_token": "ooch1Yei",
  "token_type": "bearer",
  "refresh_token": "ui0Ohch8",
  "scope": "io.cozy.files io.cozy.photos.albums",
  "two_factor_token": "123123123123",
  "two_factor_passcode": "678678"
}
```

## FranceConnect

It is pretty much the same thing as OIDC. It's logical as FranceConnect is an
OIDC provider. But we have made a special case for the login page. The
differences are that the flow is started with `GET /oidc/franceconnect`
(instead of `GET /oidc/start`) and the configuration looks like this:

```yaml
authentication:
  the-context-name:
    franceconnect:
      client_id: aClientID
      client_secret: s3cret3
      scope: openid email
      redirect_uri: https://oauthcallback.mycozy.cloud/oidc/redirect
      authorize_url: https://identity-provider/path/to/authorize
      token_url: https://identity-provider/path/to/token
      userinfo_url: https://identity-provider/path/to/userinfo
```

The last 3 URL can be omited for production.
