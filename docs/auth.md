[Table of contents](README.md#table-of-contents)

# Authentication and access delegations

## Introduction

In this document, we will cover how to protect the usage of the cozy-stack. When
the cozy-stack receives a request, it checks that the request is authorized, and
if yes, it processes it and answers it.

## What about OAuth2?

OAuth2 is about delegating an access to resources on a server to another party.
It is a framework, not a strictly defined protocol, for organizing the
interactions between these 4 actors:

-   the resource owner, the "user" that can click on buttons
-   the client, the website or application that would like to access the
    resources
-   the authorization server, whose role is limited to give tokens but is
    central in OAuth2 interactions
-   the resources server, the server that controls the resources.

For cozy, both the authorization server and the resources server roles are
played by the cozy-stack. The resource owner is the owner of a cozy instance.
The client can be the cozy-desktop app, cozy-mobile, or many other applications.

OAuth2, and its extensions, is a large world. At its core, there is 2 things:
letting the client get a token issued by the authorization server, and using
this token to access to the resources. OAuth2 describe 4 flows, called grant
types, for the first part:

-   Authorization code
-   Implicit grant type
-   Client credentials grant type
-   Resource owner credentials grant type.

On cozy, only the most typical one is used: authorization code. To start this
flow, the client must have a `client_id` and `client_secret`. The Cozy stack
implements the OAuth2 Dynamic Client Registration Protocol (an extension to
OAuth2) to allow the clients to obtain them.

OAuth2 has also 3 ways to use a token:

-   in the query-string (even if the spec does not recommended it)
-   in the POST body
-   in the HTTP Authorization header.

On cozy, only the HTTP header is supported.

OAuth2 has a lot of assumptions. Let's see some of them and their consequences
on Cozy:

-   TLS is very important to secure the communications. in OAuth 1, there was a
    mechanism to sign the requests. But it was very difficult to get it right
    for the developers and was abandonned in OAuth2, in favor of using TLS. The
    Cozy instance are already accessible only in HTTPS, so there is nothing
    particular to do for that.

-   There is a principle called TOFU, Trust On First Use. It said that if the
    user will give his permission for delegating access to its resources when
    the client will try to access them for the first time. Later, the client
    will be able to keep accessing them even if the user is no longer here to
    give his permissions.

-   The client can't make the assumptions about when its tokens will work. The
    tokens have no meaning for him (like cookies in a browser), they are just
    something it got from the authorization server and can send with its
    request. The access token can expire, the user can revoke them, etc.

-   OAuth 2.0 defines no cryptographic methods. But a developer that want to use
    it will have to put her hands in that.

If you want to learn OAuth 2 in details, I recommend the
[OAuth 2 in Action book](https://www.manning.com/books/oauth-2-in-action).

## The cozy stack as an authorization server

In general, the cozy stack manages the authentication itself. This is what is
described below. In some special cases, an integration with other softwares can
be mandatory: this is possible to configure via
[delegated authentication](./delegated-auth.md).

### GET /auth/login

Display a form with a password field to let the user authenticates herself to
the cozy stack.

This endpoint accepts a `redirect` parameter. If the user is already logged in,
she will be redirected immediately. Else, the parameter will be transfered in
the POST. This parameter can only contain a link to an application installed on
the cozy (thus to a subdomain of the cozy instance). To protect against stealing
authorization code with redirection, the fragment is always overriden:

```http
GET /auth/login?redirect=https://contacts.cozy.example.org/foo?bar#baz HTTP/1.1
Host: cozy.example.org
Cookie: ...
```

**Note**: the redirect parameter should be URL-encoded. We haven't done that to
make it clear what the path (`foo`), the query-string (`bar`), and the fragment
(`baz`) are.

```http
HTTP/1.1 302 Moved Temporarily
Location: https://contacts.cozy.example.org/foo?bar#
```

If the `redirect` parameter is invalid, the response will be `400 Bad Request`.
Same for other parameters, the redirection will happen only on success (even if
OAuth2 says the authorization server can redirect on errors, it's very
complicated to do it safely, and it is better to avoid this trap).

### POST /auth/login

After the user has typed her passphrase and clicked on `Login`, a request is
made to this endpoint.

The `redirect` parameter is passed inside the body. If it is missing, the
redirection will be made against the default target: the home application of
this cozy instance. The redirect can be a full URL (like
`http://cozy-drive.example.org/#/folder`), or just a slug+path+hash (like
`drive/#/folder`).

```http
POST /auth/login HTTP/1.1
Host: cozy.example.org
Content-Type: application/x-www-form-urlencoded

passphrase=p4ssw0rd&redirect=https%3A%2F%2Fcontacts.cozy.example.org
```

```http
HTTP/1.1 302 Moved Temporarily
Set-Cookie: ...
Location: https://contacts.cozy.example.org/foo
```

When two-factor authentication (2FA) authentication is activated, this endpoint
will not directly sent a redirection after this first passphrase step. In such
case, a `200 OK` response is sent along with a token value in the response
(either in JSON if requested or directly in a new HTML form).

Along with this token, a 2FA passcode is sent to the user via another transport
(email for instance, depending on the user's preferences). Another request
should be sent to `/auth/twofactor` with a valid pair `(token, passcode)`,
ensuring that the user correctly entered its passphrase _and_ received a fresh
passcode by another mean.

### POST /auth/twofactor

```http
POST /auth/twofactor HTTP/1.1
Host: cozy.example.org
Content-Type: application/x-www-form-urlencoded

two-factor-token=123123123123&two-factor-passcode=678678&redirect=https%3A%2F%2Fcontacts.cozy.example.org
```

```http
HTTP/1.1 302 Moved Temporarily
Set-Cookie: ...
Location: https://contacts.cozy.example.org/foo
```

### POST /auth/login/flagship

This endpoint is similar to `POST /auth/login`, but it allows the flagship app
to also obtain OAuth access and register tokens without having to make the
OAuth dance (which can be awkward for the user).

#### Request

```http
POST /auth/login/flagship HTTP/1.1
Host: alice.example.com
Content-Type: application/json
```

```json
{
  "passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791",
  "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
  "client_secret": "eyJpc3Mi[...omitted for brevity...]"
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "access_token": "OWY0MjNjMGEtOTNmNi0xMWVjLWIyZGItN2I5YjgwNmRjYzBiCg",
  "token_type": "bearer",
  "refresh_token": "YTUwMjcyYjgtOTNmNi0xMWVjLWE4YTQtZWJhMzlmMTAwMWJiCg",
  "scope": "*"
}
```

**Note:** if two-factor authentication is enabled on the Cozy, an email
will be sent to the user with a code, and this request will return:

```http
HTTP/1.1 401 Unauthorized
Content-Type: application/json
```

```json
{
  "two_factor_token": "123123123123"
}
```

Then, the client can retry by sending the two-factor token and code:

```json
{
  "passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791",
  "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
  "client_secret": "eyJpc3Mi[...omitted for brevity...]",
  "two_factor_token": "123123123123",
  "two_factor_code": "123456"
}
```

**Note:** if the two-factor authentication is enabled, and the cloudery has
already verified the email address, a parameter `email_verified_code` can be sent
to skip another 2FA code sent by mail.


```json
{
  "passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791",
  "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
  "client_secret": "eyJpc3Mi[...omitted for brevity...]",
  "email_verified_code": "987456321"
}
```

**Note:** if the OAuth client has not been certified as the flagship app,
this request will return:

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

### DELETE /auth/login

This can be used to log-out the user. An app token must be passed in the
`Authorization` header, to protect against CSRF attack on this (this can part of
bigger attacks like session fixation).

```http
DELETE /auth/login HTTP/1.1
Host: cozy.example.org
Cookie: seesioncookie....
Authorization: Bearer app-token
```

### DELETE /auth/login/others

This can be used to log-out all active sessions except the one used by the
request. This allow to disconnect any other users currenctly authenticated on
the system. An app token must be passed in the `Authorization` header, to
protect against CSRF attack on this (this can part of bigger attacks like
session fixation).

```http
DELETE /auth/login/others HTTP/1.1
Host: cozy.example.org
Cookie: seesioncookie....
Authorization: Bearer app-token
```

### POST /auth/magic_link

If the authentication via magic link is enabled on this instance, this endpoint
will send an email to the user with a magic link. If the user clicks on this
link, they will be authenticated on the Cozy.

### GET /auth/magic_link?code=...

When the user has received an email with a magic link, the link goes to the
endpoint, where the user will be allowed to enter the Cozy.

### POST /auth/magic_link/twofactor

When two-factor authentication is enabled on a Cozy, this endpoint can be used
to create a session.

### POST /auth/magic_link/flagship

This endpoint allows the flagship app to also obtain OAuth access and register
tokens without having to make the OAuth dance (which can be awkward for the
user). It requires a code sent by email to the user.

#### Request

```http
POST /auth/magic_link/flagship HTTP/1.1
Host: alice.example.com
Content-Type: application/json
```

```json
{
  "magic_code": "ODFhNzkxYTAtYjZiYi0wMTNiLTE1YzQtMThjMDRkYWJhMzI2",
  "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
  "client_secret": "eyJpc3Mi[...omitted for brevity...]"
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "access_token": "OWY0MjNjMGEtOTNmNi0xMWVjLWIyZGItN2I5YjgwNmRjYzBiCg",
  "token_type": "bearer",
  "refresh_token": "YTUwMjcyYjgtOTNmNi0xMWVjLWE4YTQtZWJhMzlmMTAwMWJiCg",
  "scope": "*"
}
```

**Note:** if two-factor authentication is enabled on the Cozy, an email
will be sent to the user with a code, and this request will return:

```http
HTTP/1.1 401 Unauthorized
Content-Type: application/json
```

```json
{
  "error": "passphrase is required as second authentication factor"
}
```

Then, the client can retry by sending the two-factor token and code:

```json
{
  "magic_code": "ODFhNzkxYTAtYjZiYi0wMTNiLTE1YzQtMThjMDRkYWJhMzI2",
  "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
  "client_secret": "eyJpc3Mi[...omitted for brevity...]",
  "passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791"
}
```

**Note:** if the OAuth client has not been certified as the flagship app,
this request will return:

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

### GET /auth/passphrase_reset

Display a form for the user to reset its password, in case he has forgotten it
for example. If the user is connected, he won't be shown this form and he will
be directly redirected to his cozy.

This endpoint accepts a `hideBackButton` parameter. If this parameter is present
and set to `true` then the passphrase reset page won't display any button to go
back to the login page.
This is useful when this page is opened in a different context from the one in
which the login page was opened (e.g. a browser vs a mobile native application).

It is also possible to use `from=settings` parameter in the query-string, to go
back to the settings app after the password has been reset. It is useful when
the user wants to change their email address, as the process for changing this
address requires the password.

```http
GET /auth/passphrase_reset?hideBackButton=true HTTP/1.1
Host: cozy.example.org
Content-Type: text/html
Cookie: ...
```

### POST /auth/hint

Send the password hint by email.

```http
POST /auth/hint HTTP/1.1
Host: cozy.example.org
```

### POST /auth/onboarding/resend

Resend the activation link by email to finalize the onboarding.

```http
POST /auth/onboarding/resend HTTP/1.1
Host: cozy.example.org
```

### POST /auth/passphrase_reset

After the user has clicked on the reset button of the passphrase reset form, it
will execute a request to this endpoint.

This endpoint will create a token for the user to actually renew his passphrase.
The token has a short-live duration of about 15 minutes. After the token is
created, it is sent to the user on its mailbox.

This endpoint will redirect the user on the login form page.

```http
POST /auth/passphrase_reset HTTP/1.1
Host: cozy.example.org
Content-Type: application/x-www-form-urlencoded

csrf_token=123456890
```

### GET /auth/passphrase_renew

Display a form for the user to enter a new password. This endpoint should be
used with a `token` query parameter. This token makes sure that the user has
actually reset its passphrase and should have been sent via its mailbox.

```http
GET /auth/passphrase_renew?token=123456789 HTTP/1.1
Host: cozy.example.org
Content-Type: text/html
Cookie: ...
```

### POST /auth/passphrase_renew

After the user has entered its new passphrase in the renew passphrase form, a
request is made to this endpoint to renew the passphrase.

This endpoint requires a valid token to actually work. In case of a success, the
user is redirected to the login form.

```http
POST /auth/passphrase_reset HTTP/1.1
Host: cozy.example.org
Content-Type: application/x-www-form-urlencoded

csrf_token=123456890&passphrase_reset_token=123456789&passphrase=mynewpassphrase
```

#### Parameters

| Form parameter         | Description                               |
| ---------------------- | ----------------------------------------- |
| passphrase_reset_token | the token to authenticate the request     |
| csrf_token             | the token to protect against CSRF attacks |
| passphrase             | the new password                          |
| hint                   | the hint to find again the password       |
| iterations             | the number of PBKDF2 iterations           |
| key                    | the encrypted master key for bitwarden    |
| public_key             | the public key for the cozy organization  |
| private_key            | the private key for the cozy organization |

### GET /auth/passphrase

This page renders a form to set the password for an onboarding instance. This
endpoint expects a valid `registerToken` parameter.

In case of success, the instance is marked as onboarded and the user is
redirected to his home.

If the instance is already onboarded, the user is redirected to his home.

It is also possible to give a `redirection` parameter to redirect the user to
another application.

```http
GET /auth/passphrase/?registerToken=e0fbe2c5b90cdcdd9b3487b48b480e0b&redirection=drive/%23/files HTTP/1.1
Host: cozy.example.org
Content-Type: text/html
```

### GET /auth/confirm

An application can ask the user to re-authenticate them-selves before making an
important action (like erasing a pin code). To do that, the application will
sent the user to this page where the stack will show a form.

Two parameters in the query string can be sent:

- `state` (mandatory), which can be seen as an identifier for the confirmation
- `redirect` (optional), where the user will be redirected after the confirmation.

```http
GET /auth/confirm?state=51814f30-5818-0139-9348-543d7eb8149c&redirect=http://banks.cozy.localhost:8080/ HTTP/1.1
```

The application can know the user has confirmed their identity by subscribing
to a real-time event or by looking at the URL after the redirection. The URL
must contain the state given by the app, and a code that can be checked by
calling `POST /auth/confirm/code` (see below).

### POST /auth/confirm

Send the hashed password for confirming the authentication.

#### Redirection

```http
HTTP/1.1 302 Moved Temporarily
Location: http://banks.cozy.localhost:8080/?state=51814f30-5818-0139-9348-543d7eb8149c&code=543d7eb8149c
```

#### Real-time via websockets

If it succeeds, a real-time event will be sent:

```
client > {"method": "AUTH",
          "payload": "xxAppOrAuthTokenxx="}
client > {"method": "SUBSCRIBE",
          "payload": {"type": "io.cozy.auth.confirmations"}}
server > {"event": "CREATED",
          "payload": {"id": "51814f30-5818-0139-9348-543d7eb8149c",
                      "type": "io.cozy.auth.confirmations"}}
```

### GET /auth/confirm/:code

Send the code from the URL to check that the user has really confirmed their
identity (and not just typed the URL them-self).

```http
GET /auth/confirm/543d7eb8149c HTTP/1.1
```

The response will be a 204 No Content if the code is valid (and a 401 else).

### POST /auth/register

This route is used by OAuth2 clients to dynamically register them-selves.

See
[OAuth 2.0 Dynamic Client Registration Protocol](https://tools.ietf.org/html/rfc7591)
for the details.

The client must send a JSON request, with at least:

-   `redirect_uris`, an array of strings with the redirect URIs that the client
    will use in the authorization flow
-   `client_name`, human-readable string name of the client to be presented to
    the end-user during authorization
-   `software_id`, an identifier of the software used by the client (it should
    remain the same for all instances of the client software, whereas
    `client_id` varies between instances).

It can also send the optional fields:

-   `client_kind` (possible values: web, desktop, mobile, browser, etc.)
-   `client_uri`, URL string of a web page providing information about the
    client
-   `logo_uri`, to display an icon to the user in the authorization flow
-   `policy_uri`, URL string that points to a human-readable privacy policy
    document that describes how the deployment organization collects, uses,
    retains, and discloses personal data
-   `software_version`, a version identifier string for the client software.
-   `notification_platform`, to activate notifications on the associated device,
    this field specify the platform used to send notifications:
    - `"android"`: for Android devices with notifications via Firebase Cloud
      Messaging
    - `"ios"`: for iOS devices with notifications via Firebase Cloud
      Messaging or APNS/2
    - `"huawei"`: for huawei devices with Push Kit
-   `notification_device_token`, the token used to identify the mobile device
    for notifications.

The server gives to the client the previous fields and these informations:

-   `client_id`
-   `client_secret`
-   `registration_access_token`

Example:

```http
POST /auth/register HTTP/1.1
Host: cozy.example.org
Content-Type: application/json
Accept: application/json
```

```json
{
    "redirect_uris": ["https://client.example.org/oauth/callback"],
    "client_name": "Client",
    "software_id": "github.com/example/client",
    "software_version": "2.0.1",
    "client_kind": "web",
    "client_uri": "https://client.example.org/",
    "logo_uri": "https://client.example.org/logo.svg",
    "policy_uri": "https://client/example.org/policy"
}
```

```http
HTTP/1.1 201 Created
Content-Type: application/json
```

```json
{
    "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
    "client_secret": "eyJpc3Mi[...omitted for brevity...]",
    "client_secret_expires_at": 0,
    "registration_access_token": "J9l-ZhwP[...omitted for brevity...]",
    "grant_types": ["authorization_code", "refresh_token"],
    "response_types": ["code"],
    "redirect_uris": ["https://client.example.org/oauth/callback"],
    "client_name": "Client",
    "software_id": "github.com/example/client",
    "software_version": "2.0.1",
    "client_kind": "web",
    "client_uri": "https://client.example.org/",
    "logo_uri": "https://client.example.org/logo.svg",
    "policy_uri": "https://client/example.org/policy"
}
```


### POST /auth/registerFromWebApp

This route is similar to `POST /auth/register` but is restricted to the Settings
application only. It uses the same OAuth2 Dynamic Client Registration Protocol
and accepts the same request/response format as `POST /auth/register`.

**Important:** This endpoint requires authentication from the Settings webapp. The
request must include a valid token from the Settings application, otherwise it
will return a 403 Forbidden error.

Example:

```http
POST /auth/registerFromWebApp HTTP/1.1
Host: cozy.example.org
Content-Type: application/json
Accept: application/json
Authorization: Bearer <settings-app-token>
```

The request and response format are identical to `POST /auth/register`.


#### Linked applications

Some OAuth applications are a mobile or desktop version of a Cozy webapp. For
them, it's possible to use a special `software_id` to make a link between the
mobile/desktop application and the webapp. The `software_id` must be in a
`registry://slug` format, like `registry://drive` for Cozy-Drive for example.
When it is the case, the mobile/desktop will share its permissions with the
webapp. It means several things:

- The mobile/desktop app will have the same permissions that the linked webapp.
- When a sharing by link is done in the mobile/desktop application, the linked
  webapp will be able to revoke it later, and vice versa.
- When the user accepts to give access to its Cozy to the mobile/desktop app,
  the linked webapp will be installed on the Cozy if it was not already the
  case.
- When the linked webapp is uninstalled, the right to access the Cozy for the
  mobile/desktop app will be revoked.

### GET /auth/register/:client-id

This route is used by the clients to get informations about them-selves. The
client has to send its registration access token to be able to use this
endpoint.

See
[OAuth 2.0 Dynamic Client Registration Management Protocol](https://tools.ietf.org/html/rfc7592)
for more details.

```http
GET /auth/register/64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3 HTTP/1.1
Host: cozy.example.org
Accept: application/json
Authorization: Bearer J9l-ZhwP...
```

```http
HTTP/1.1 201 Created
Content-Type: application/json
```

```json
{
    "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
    "client_secret": "eyJpc3Mi[...omitted for brevity...]",
    "client_secret_expires_at": 0,
    "grant_types": ["authorization_code", "refresh_token"],
    "response_types": ["code"],
    "redirect_uris": ["https://client.example.org/oauth/callback"],
    "client_name": "Client",
    "software_id": "github.com/example/client",
    "software_version": "2.0.1",
    "client_kind": "web",
    "client_uri": "https://client.example.org/",
    "logo_uri": "https://client.example.org/logo.svg",
    "policy_uri": "https://client/example.org/policy"
}
```

### PUT /auth/register/:client-id

This route is used by the clients to update informations about them-selves. The
client has to send its registration access token to be able to use this
endpoint.

**Note:** the client can ask to change its `client_secret`. To do that, it must
send the current `client_secret`, and the server will respond with the new
`client_secret`.

```http
PUT /auth/register/64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3 HTTP/1.1
Host: cozy.example.org
Accept: application/json
Content-Type: application/json
Authorization: Bearer J9l-ZhwP...
```

```json
{
    "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
    "client_secret": "eyJpc3Mi[...omitted for brevity...]",
    "redirect_uris": ["https://client.example.org/oauth/callback"],
    "client_name": "Client",
    "software_id": "github.com/example/client",
    "software_version": "2.0.2",
    "client_kind": "web",
    "client_uri": "https://client.example.org/",
    "logo_uri": "https://client.example.org/client-logo.svg",
    "policy_uri": "https://client/example.org/policy",
    "notification_platform": "android",
    "notification_device_token": "XXXXxxxx..."
}
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
    "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
    "client_secret": "IFais2Ah[...omitted for brevity...]",
    "client_secret_expires_at": 0,
    "grant_types": ["authorization_code", "refresh_token"],
    "response_types": ["code"],
    "redirect_uris": ["https://client.example.org/oauth/callback"],
    "client_name": "Client",
    "software_id": "github.com/example/client",
    "software_version": "2.0.2",
    "client_kind": "web",
    "client_uri": "https://client.example.org/",
    "logo_uri": "https://client.example.org/client-logo.svg",
    "policy_uri": "https://client/example.org/policy"
}
```

### DELETE /auth/register/:client-id

This route is used by the clients to unregister them-selves. The client has to
send its registration access token to be able to use this endpoint.

```http
DELETE /auth/register/64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3 HTTP/1.1
Host: cozy.example.org
Authorization: Bearer J9l-ZhwP...
```

```http
HTTP/1.1 204 No Content
```

### POST /auth/clients/:client-id/challenge

This route can be used to start the process for certifying that an app is
really what it tells to be by using the android/iOS APIs
(PlayIntegrity/SafetyNet/AppleAttestation). It returns a nonce that must be
used in the certificate.

The client must send its registration access token to use this endpoint.

```http
POST /auth/clients/64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3/challenge HTTP/1.1
Host: cozy.example.org
Authorization: Bearer J9l-ZhwP...
```

```http
HTTP/1.1 201 Created
Content-Type: application/json
```

```json
{
  "nonce": "MmE3OTM1ZDItNWY0ZC0xMWVjLTg3NT"
}
```

### POST /auth/clients/:client-id/attestation

This route can be used to finish the process for certifying that an app is
really what it tells to be by using the android/iOS APIs. The client can send
its attestation.

```http
POST /auth/clients/64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3/attestation HTTP/1.1
Host: cozy.example.org
Content-Type: application/json
```

```json
{
  "platform": "android",
  "challenge": "MmE3OTM1ZDItNWY0ZC0xMWVjLTg3NT",
  "attestation": "eyJ0eXAiOiJKV1QiLA0KICJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJqb2UiLA0KICJleHAiOjEzMDA4MTkzODAsDQogImh0dHA6Ly9leGFtcGxlLmNvbS9pc19yb290Ijp0cnVlfQ.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
}
```

Note: the `platform` parameter can be `"android"` or `"ios"`. For `ios`, a
`"keyId"` parameter is also required. For `android`, the `"issuer"` can be
`"playintegrity"` to use the Play Integrity API instead of the SafetyNet API.

```http
HTTP/1.1 204 No Content
```

### POST /auth/clients/:client-id/flagship

This route can be used to send a 6-digits code to manually certify a client as
belonging to the flagship app.

```http
POST /auth/clients/64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3/flagship HTTP/1.1
Host: cozy.example.org
Content-Type: application/x-www-form-urlencoded

code=123456&token=123123123123123
```

```http
HTTP/1.1 204 No Content
```

### GET /auth/authorize

When an OAuth2 client wants to get access to the data of the cozy owner, it
starts the OAuth2 dance with this step. The user is shown what the client asks
and has an accept button if she is OK with that.

In case a limit has been set on the Cozy to the number of user-connected OAuth
clients, and this limit has been reached already, the user will be presented a
screen requesting to either remove some existing clients or, if enabled,
increase the limit (e.g. by subscribing to a plan with a greater limit). Once
the number of connected clients is brought back under the limit, the OAuth flow
will resume and the permissions screen will be displayed.

The parameters are:

-   `client_id`, that identify the client
-   `redirect_uri`, it has to be exactly the same as the one used in
    registration
-   `state`, it's a protection against CSRF on the client (a random string
    generated by the client, that it can check when the user will be redirected
    with the authorization code. It can be used as a key in local storage for
    storing a state in a SPA).
-   `response_type`, only `code` is supported
-   `scope`, a space separated list of the [permissions](permissions.md) asked
    (like `io.cozy.files:GET` for read-only access to files).

```http
GET /auth/authorize?client_id=oauth-client-1&response_type=code&scope=io.cozy.files%3AGET%20io.cozy.contacts&state=Eh6ahshepei5Oojo&redirect_uri=https%3A%2F%2Fclient.org%2F HTTP/1.1
Host: cozy.example.org
```

**Note** we warn the user that he is about to share his data with an application
which only the callback URI is guaranteed.

#### PKCE extension

To improve security, the client can use the [PKCE for OAuth
2](https://oauth.net/2/pkce/). In that case, two additional parameters must be
send to `GET /auth/authorize`:

- `code_challenge`: the client creates a `code_verifier`, and then derive the
  `code_challenge` from it.
- `code_challenge_method`: it must be `S256` (the only supported method).

As a reminder, the relation between `code_verifier` and `code_challenge` is the
following:

```
code_challenge = BASE64URL-ENCODE(SHA256(code_verifier))
```

And, the `code_verifier` parameter must be sent to `POST /auth/access_token`
(see below).

### POST /auth/authorize

When the user accepts, her browser send a request to this endpoint:

```http
POST /auth/authorize HTTP/1.1
Host: cozy.example.org
Content-Type: application/x-www-form-urlencoded

state=Eh6ahshepei5Oojo&client_id=oauth-client-1&scope=io.cozy.files%3AGET%20io.cozy.contacts&csrf_token=johw6Sho&response_type=code&redirect_uri=https%3A%2F%2Fclient.org%2F
```

**Note**: this endpoint is protected against CSRF attacks.

The user is then redirected to the original client, with an access code in the
URL:

```http
HTTP/1.1 302 Moved Temporarily
Location: https://client.org/?state=Eh6ahshepei5Oojo&code=Aih7ohth#
```

### GET /auth/authorize/sharing & POST /auth/authorize/sharing

They are similar to `/auth/authorize`: they also make the user accept an OAuth
thing, but it is specialized for sharing. They are a few differences, like the
scope format (sharing rules, not permissions) and the redirection after the
POST (with `sharing=<sharing-id>` in the query string).

### GET /auth/authorize/move & POST /auth/authorize/move

They are similar to `/auth/authorize`, but instead of a page that lists the
permissions, the user will be asked to type their password to confirm the
action (even if they are already logged-in). If the 2FA is activated, the code
will be asked too. This strong action seems adequate for authorizing to erase
the data in the Cozy before importing data from another Cozy.

#### Request GET

```http
GET /auth/authorize/move?state=8d560d60&client_id=oauth-client-2&redirect_uri=https://move.cozycloud.cc/callback/target HTTP/1.1
Server: target.cozy.example
```

#### Response GET

```http
HTTP/1.1 200 OK
Content-Type: application/html
```

#### Request POST

```http
POST /auth/authorize/move HTTP/1.1
Server: target.cozy.example
Content-Type: application/x-www-form-urlencoded

passphrase=hashed&state=8d560d60&client_id=oauth-client-2&csrf_token=johw6Sho&redirect_uri=https%3A%2F%2Fmove.cozycloud.cc%2Fcallback%2Ftarget
```

#### Response POST

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "redirect": "https://move.cozycloud.cc/callback/target?code=543d7eb8149c&used=123456&quota=5000000&state=8d560d60&vault=true"
}
```

### POST /auth/access_token

Now, the client can check that the state is correct, and if it is the case, ask
for an `access_token`. It can use this route with the `code` given above.

This endpoint is also used to refresh the access token, by sending the
`refresh_token` instead of the `code`.

The parameters are:

-   `grant_type`, with `authorization_code` or `refresh_token` as value
-   `code` or `refresh_token`, depending on which grant type is used
-   `client_id`
-   `client_secret`

Example:

```http
POST /auth/access_token HTTP/1.1
Host: cozy.example.org
Content-Type: application/x-www-form-urlencoded
Accept: application/json

grant_type=authorization_code&code=Aih7ohth&client_id=oauth-client-1&client_secret=Oung7oi5
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
  "scope": "io.cozy.files:GET io.cozy.contacts"
}
```

### POST /auth/token_exchange

This endpoint exchanges an external OIDC `id_token` for a normal Cozy OAuth
client and token pair on the target instance.

It supports two configured exchanges:

- admin applications that authenticate with an external identity provider, then
  need to call Cozy APIs directly on an organization instance
- trusted user applications mapped by OIDC audience to a Cozy linked app, such
  as `twake-mail-web` to `registry://mail`

The target Cozy instance is the request host. The exchanged `id_token` must:

- be signed by the configured OIDC provider
- match the configured issuer and audience
- contain `iat` and `exp` claims
- contain a `sid` claim so the created OAuth client can be revoked by OIDC
  backchannel logout

For admin exchanges, the `id_token` must also:

- contain an `org_id` claim equal to the target instance organization id
- contain an `org_role` claim equal to `owner` or `admin`

For user app exchanges, the `id_token` audience must be present in the
`oidc.app_token_exchange` stack configuration. The token must also match the
target Cozy instance. When the matched app exchange entry sets `instance_claim`,
that claim must contain the target instance domain. Otherwise the existing OIDC
instance mapping is used (`sub` to `oidc_id` when `allow_custom_instance` is
true, otherwise `userinfo_instance_field` with prefix/suffix). The returned
scope is derived from the configured linked app manifest, for example
`@io.cozy.apps/mail`; caller-provided scopes are rejected.

The request body is JSON:

- `id_token`, the external OIDC token
- `exchange_type`, optional, either `admin` or `app`; when omitted, `admin`
  is used
- `scope`, for admin exchanges only, a space-separated list of allowed
  doctypes: `io.cozy.files`, `io.cozy.contacts`, `io.cozy.contacts.groups`

Example:

```http
POST /auth/token_exchange HTTP/1.1
Host: myorg123.example.com
Content-Type: application/json
Accept: application/json

{
  "id_token": "eyJhbGciOiJSUzI1NiIsImtpZCI6InRva2VuLWV4Y2hhbmdlIn0...",
  "scope": "io.cozy.files"
}
```

Example app exchange:

```json
{
  "id_token": "eyJhbGciOiJSUzI1NiIsImtpZCI6InRva2VuLWV4Y2hhbmdlIn0...",
  "exchange_type": "app"
}
```

Response:

```http
HTTP/1.1 200 OK
Content-Type: application/json
Cache-Control: no-store
Pragma: no-cache
```

```json
{
  "access_token": "eyJhbGciOiJS",
  "token_type": "bearer",
  "refresh_token": "eyJhbGciOiJS",
  "scope": "io.cozy.files",
  "client_id": "64ce5cb0-bd4c-11e6-880e-b3b7dfda89d3",
  "client_secret": "Oung7oi5",
  "registration_access_token": "reg123"
}
```

The returned OAuth client is a normal Cozy OAuth client:

- `client_id`, `client_secret`, `access_token`, and `refresh_token` can be
  used directly with `cozy-client`
- `registration_access_token` can be used with
  `DELETE /auth/register/:client-id` to revoke that exchanged client

The created OAuth client is bound to the upstream OIDC session so it can be
revoked by OIDC backchannel logout.

### POST /auth/session_code

This endpoint can be used by the flagship application in order to create a
session code: this code can be added to the URL of a cozy application (in the
query string, as `session_code`) to create a session. The flagship can create
this code with its access token, and then use it in a webview to avoid the
reauthentication of the user. It can also create the code with the hashed
passphrase (and 2FA if needed) to create a session for the authorize page.

Note that the difference between a `session_code` and a `magic_code` (code in a
magic link sent by email) is the behavior when two-factor authentication is
enabled. The `session_code` will open the session while the `magic_code` will
require the password for that.

#### Request (access token variant)

```http
POST /auth/session_code HTTP/1.1
Host: cozy.example.org
Accept: application/json
Authorization: Bearer eyJpc3Mi...
```

#### Request (passphrase variant)

```http
POST /auth/session_code HTTP/1.1
Host: cozy.example.org
Accept: application/json
Content-Type: application/json
```

```json
{
  "passphrase": "hashed",
  "two_factor_token": "123123123123",
  "two_factor_passcode": "678678"
}
```

#### Response

```http
HTTP/1.1 201 Created
Content-Type: application/json
```

```json
{
  "session_code": "HzEFM3JREpIB6532fQc1FP2t4YJKt3gI"
}
```

The flagship will then be able to open a webview for
`https://cozy-home.example.org/?session_code=HzEFM3JREpIB6532fQc1FP2t4YJKt3gI`.

#### Response (2FA needed)

In case of error where 2FA is needed, the response will be:

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

### FAQ

> What format is used for tokens?

The access tokens are formatted as [JSON Web Tokens (JWT)](https://jwt.io/),
like this:

| Claim   | Fullname  | What it identifies                                                 |
| ------- | --------- | ------------------------------------------------------------------ |
| `aud`   | Audience  | Identify the recipient where the token can be used (like `access`) |
| `iss`   | Issuer    | Identify the Cozy instance (its domain in fact)                    |
| `iat`   | Issued At | Identify when the token was issued (Unix timestamp)                |
| `sub`   | Subject   | Identify the client that can use the token                         |
| `scope` | Scope     | Identify the scope of actions that the client can accomplish       |

The `scope` is used for [permissions](permissions.md).

Other tokens can be JWT with a similar formalism, or be a simple random value
(when we want to have a clear revocation process).

> What happens when the user has lost her passphrase?

She can reset it from the command-line, like this:

```sh
$ cozy-stack instances reset-passphrase cozy.example.org
ek0Jah1R
```

A new password is generated and print in the console.

> Is two-factor authentication (2FA) possible?

Yes, it's possible. Via the cozy-settings application, the two-factor
authentication can be activated.

Here is how it works in more details:

On each connection, when the 2FA is activated, the user is asked for its
passphrase first. When entering correct passphrase, the user is then asked for:

-   a TOTP (Timebased One-Time password, RFC 6238) derived from a secret
    associated with the instance.
-   a short term timestamped MAC with the same validity time-range and also
    derived from the same secret.

The TOTP is valid for a time range of about 5 minutes. When sending a correct
and still-valid pair `(passcode, token)`, the user is granted with
authentication cookie.

The passcode can be sent to the instance's owner via email — more transport
shall be added later.

### POST /auth/tokens/konnectors/:slug

This endpoint can be used by the flagship application in order to create a
token for the konnector with the given slug. This token can then be used by the
client-side konnector to make requests to cozy-stack.
The flagship app will need to use its own access token to request the konnector 
token.

#### Request

```http
POST /auth/tokens/konnectors/impots HTTP/1.1
Host: cozy.example.org
Accept: application/json
Authorization: Bearer eyJpc3Mi...
```

#### Response

```http
HTTP/1.1 201 Created
Content-Type: application/json
```

```json
"OWY0MjNjMGEtOTNmNi0xMWVjLWIyZGItN2I5YjgwNmRjYzBiCg"
```

### POST /auth/share-by-link/password

This route is used when a share by link is protected by password. The password
can be sent to this route to create a cookie that will allow to use the
sharecode and access the shared page.

#### Request

```http
POST /auth/clients/share-by-link/password HTTP/1.1
Host: cozy.example.org
Content-Type: application/x-www-form-urlencoded

password=HelloWorld!&perm_id=123456789
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
Set-Cookie: pass123...
```

```json
{
  "password": "ok"
}
```

### FAQ

> What format is used for tokens?

The access tokens are formatted as [JSON Web Tokens (JWT)](https://jwt.io/),
like this:

| Claim   | Fullname  | What it identifies                                                      |
| ------- | --------- | ----------------------------------------------------------------------- |
| `aud`   | Audience  | Identify the recipient where the token can be used (i.e. `konn`)        |
| `iss`   | Issuer    | Identify the Cozy instance (its domain in fact)                         |
| `iat`   | Issued At | Identify when the token was issued (Unix timestamp)                     |
| `sub`   | Subject   | Identify the client that can use the token (i.e. the konnector slug)    |
| `scope` | Scope     | Konnector tokens don't have any scope                                   |


## Client-side apps

**Important**: OAuth2 is not used here! The steps looks similar (like obtaining
a token), but when going in the details, it doesn't match.

### How to register the application?

The application is registered at install. See [app management](apps.md) for
details.

### How to get a token?

When a user access an application, she first loads the HTML page. Inside this
page, a token specific to this app is injected (only for private routes), via a
templating method.

We have prefered our custom solution to the implicit grant type of OAuth2 for 2
reasons:

1. It has a better User Experience. The implicit grant type works with 2
   redirections (the application to the stack, and then the stack to the
   application), and the first one needs JS to detect if the token is present or
   not in the fragment hash. It has a strong impact on the time to load the
   application.

2. The implicit grant type of OAuth2 has a severe drawback on security: the
   token appears in the URL and is shown by the browser. It can also be leaked
   with the HTTP `Referer` header.

The token will be given only for the authenticated user. For nested subdomains
(like `calendar.joe.example.net`), the session cookie from the stack is enough
(it is for `.joe.example.net`).

But for flat subdomains (like `joe-calendar.example.net`), it's more
complicated. On the first try of the user, she will be redirected to the stack.
As she is already logged-in, she will be redirected to the app with a session
code (else she can login). This session code can be exchanged to a session
cookie. A redirection will still happen to remove the code from the URL (it
helps to avoid the code being saved in the browser history). For security
reasons, the session code have the following properties:

-   It can only be used once.
-   It is tied to an application (`calendar` in our example).
-   It has a very short time span of validity (1 minute).

### How to use a token?

The token can be sent to the cozy-stack as a `Bearer` token in the
`Authorization` header, like this:

```http
GET /data/io.cozy.events/6494e0ac-dfcb-11e5-88c1-472e84a9cbee HTTP/1.1
Host: cozy.example.org
Authorization: Bearer application-token
```

If the user is authenticated, her cookies will be sent automatically. The
cookies are needed for a token to be valid.

### How to refresh a token?

The token is valid only for 24 hours. If the application is opened for more than
that, it will need to get a new token. But most applications won't be kept open
for so long and it's okay if they don't try to refresh tokens. At worst, the
user just had to reload its page and it will work again.

The app can know it's time to get a new token when the stack starts sending 401
Unauthorized responses. In that case, it can fetches the same html page that it
was loaded initially, parses it and extracts the new token.

## Third-party websites

### How to register the application?

If a third-party websites would like to access a cozy, it had to register first.
For example, a big company can have data about a user and may want to offer her
a way to get her data back in her cozy. When the user is connected on the
website of this company, she can give her cozy address. The website will then
register on this cozy, using the OAuth2 Dynamic Client Registration Protocol, as
explained [above](#post-authregister).

### How to get a token?

To get an access token, it's enough to follow the authorization code flow of
OAuth2:

-   sending the user to the cozy, on the authorize page
-   if the user approves, she is then redirected back to the client
-   the client gets the access code and can exchange it to an access token.

### How to use a token?

The access token can be sent as a bearer token, in the Authorization header of
HTTP:

```http
GET /data/io.cozy.contacts/6494e0ac-dfcb-11e5-88c1-472e84a9cbee HTTP/1.1
Host: cozy.example.org
Accept: application/json
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
```

### How to refresh a token?

The access token will be valid only for 24 hours. After that, a new access token
must be asked. To do that, just follow the refresh token flow, as explained
[above](#post-authaccess_token).

## Devices and browser extensions

For devices and browser extensions, it is nearly the same than for third-party
websites. The main difficulty is the redirect_uri. In OAuth2, the access code is
given to the client by redirecting the user to an URL controlled by the client.
But devices and browser extensions don't have an obvious URL for that.

The IETF has published an RFC called
[OAuth 2.0 for Native Apps](https://tools.ietf.org/html/draft-ietf-oauth-native-apps-05).

### Native apps on desktop

A desktop native application can start an embedded webserver on localhost. The
redirect_uri will be something like `http://127.0.0.1:19856/callback`.

### Native apps on mobile

On mobile, the native apps can often register a custom URI scheme, like
`com.example.oauthclient:/`. Just be sure that no other app has registered
itself with the same URI.

### Chrome extensions

Chrome extensions can use URL like
`https://<extension-id>.chromiumapp.org/<anything-here>` for their usage. See
https://developer.chrome.com/apps/app_identity#non for more details. It has also
a method to simplify the creation of such an URL:
[`chrome.identity.getRedirectURL`](https://developer.chrome.com/apps/identity#method-getRedirectURL).

### Firefox extensions

It is possible to use an _out of band_ URN: `urn:ietf:wg:oauth:2.0:oob:auto`.
The token is then extracted from the title of the page. See
[this addon for google oauth2](https://github.com/AdrianArroyoCalle/firefox-addons/blob/master/addon-google-oauth2/addon-google-oauth2.js)
as an example.

## Security considerations

The master password, the password known by the user, is derived on the clients
to give two keys. The first key is used to login on the stack, the second key
is used to do client-side encryption. The derivation for the login password is
currently done with the PBKDF2 algorithm (with SHA256), but we have anticipated
the possibility of changing to another algorithm if desirable.

The derived password is stored on the server in a secure fashion, with a
password hashing function. The hashing function and its parameter are stored
with the hash, in order to make it possible to change the algorithm and/or the
parameters later if we had any suspicion that it became too weak. The initial
algorithm is [scrypt](https://pkg.go.dev/golang.org/x/crypto/scrypt).

The access code is valid only once, and will expire after 5 minutes

Dynamically registered applications won't have access to all possible scopes.
For example, an application that has been dynamically registered can't ask the
cozy owner to give it the right to install other applications. This limitation
should improve security, as avoiding too powerful scopes to be used with unknown
applications.

The cozy stack will apply rate limiting to avoid brute-force attacks.

The cozy stack offers
[CORS](https://developer.mozilla.org/en-US/docs/Web/HTTP/Access_control_CORS)
for most of its services. But it's disabled for `/auth` (it doesn't make sense
here) and for the client-side applications (to avoid leaking their tokens).

The client should really use HTTPS for its `redirect_uri` parameter, but it's
allowed to use HTTP for localhost, as in the native desktop app example.

OAuth2 says that the `state` parameter is optional in the authorization code
flow. But it is mandatory to use it with Cozy.

For more on this subject, here is a list of links:

-   https://www.owasp.org/index.php/Authentication_Cheat_Sheet
-   https://tools.ietf.org/html/rfc6749#page-53
-   https://tools.ietf.org/html/rfc6819
-   https://tools.ietf.org/html/draft-ietf-oauth-closing-redirectors-00
-   http://www.oauthsecurity.com/

## Conclusion

Security is hard. If you want to share some concerns with us, do not hesitate to
send us an email to security AT cozycloud.cc.
