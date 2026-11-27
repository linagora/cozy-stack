[Table of contents](README.md#table-of-contents)

# Settings

## Disk usage


### GET /settings/disk-usage

Says how many bytes are available and used to store files. When not limited the
`quota` field is omitted. Also says how many bytes are used by last version of
files and how many bytes are taken by older versions.

If the `include=trash` parameter is added to the query string, it will also
compute the size of the files in the trash.

#### Request

```http
GET /settings/disk-usage?include=trash HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.disk-usage",
        "attributes": {
            "quota": "123456789",
            "used": "12345678",
            "files": "10305070",
            "trash": "456789",
            "versions": "2040608"
        }
    }
}
```

## OAuth clients usage

### GET /settings/clients-usage

This endpoint returns the number of user-connected OAuth clients, the limit set
on the Cozy and if this limit has been reached or even exceeded.
If there is no limit, the `limit` attribute won't be present in the response.

#### Request

```
GET /settings/clients-usage HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.clients-usage",
        "attributes": {
          "limit": 3,
          "count": 3,
          "limitReached": true,
          "limitExceeded": false
        }
    }
}
```


## Email update

### POST /settings/email

The email adress update process starts with this call. The password is required
in order to make a strong authentication. This endpoint will send a confirmation
email to the new address with a link. Once clicked, this link will redirect the
user to the second endpoint.

#### Request

```http
POST /settings/email HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Authorization: Bearer ...
```

```json
{
    "passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791",
    "email": "alice@example.com"
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /settings/email/resend

Once the email process is started, it's possible to resend an email in order to ensure
the link delivery.

#### Request

```http
POST /settings/email HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 204 No Content
```

### GET /settings/email/confirm

This is the second part of the email update process. The user have received a
confirmation email with a link on its new email adress. When he click on the link
he ends up on this endpoint. The url contains a token used to authenticate the user
and the action.

#### Request

```http
GET /settings/email/confirm?token=XXXXXXX HTTP/1.1
Host: alice.example.com
```

#### Response

In case of success the user will be redirected to its setting page. In case of error
an HTML error page will appears.

```http
HTTP/1.1 307 Temporary Redirect
Location: http://alice-settings.cozy.localhost:8080
```


### DELETE /settings/email

This endpoints allows to cancel the ongoing email adress update process.

#### Request

```http
DELETE /settings/email HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 204 No Content
```


## Passphrase

The master password, known by the cozy owner, is used for two things: to allow
the user to login and to do encryption on the client side. To do so, two keys
are derivated from the master password, one for each usage. In this section, we
are talking about the derivated key used for login on a cozy instance.

### GET /settings/passphrase

The server will send the parameters for hashing the master password on the
client side to derive a key used for login.

Note: a permission on `GET io.cozy.settings` is required for this endpoint.

#### Request

```http
GET /settings/passphrase HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.passphrase",
        "attributes": {
            "salt": "me@alice.example.com",
            "kdf": 0,
            "iterations": 10000
        }
    }
}
```

Note: only `kdf: 0` is currently supported. It means PBKDF2 with SHA256.

### POST /settings/passphrase (form)

The user can send its new hashed passphrase (base64 encoded) to finish the
onboarding. The registration token can only be used once.

The `key` is the encryption key for bitwarden, encrypted with the master key.
The `public_key` and `private_key` are the key pair for sharing data with a
bitwarden organization, and they are optional.

#### Request

```http
POST /settings/passphrase HTTP/1.1
Host: alice.example.com
Content-Type: application/x-www-form-urlencoded

register_token=37cddf40d7724988860fa0e03efd30fe&
passphrase=4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791&
key=0.uRcMe+Mc2nmOet4yWx9BwA==|PGQhpYUlTUq/vBEDj1KOHVMlTIH1eecMl0j80+Zu0VRVfFa7X/MWKdVM6OM/NfSZicFEwaLWqpyBlOrBXhR+trkX/dPRnfwJD2B93hnLNGQ=&
public_key=MIIBIjANBgkqhkiG9w...AQAB&
private_key=2.wZuKkufLV31Cpw1v1TQUDA==|u6bUNTaaGxu...y7s=&
iterations=100000
```

#### Response

```http
HTTP/1.1 303 See Other
Set-Cookie: cozysessid=AAAAAFhSXT81MWU0ZTBiMzllMmI1OGUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa; Path=/; Domain=alice.example.com; Max-Age=604800; HttpOnly; Secure
Location: https://alice-home.example.com/
```

### POST /settings/passphrase (json)

The onboarding application can send a request to this endpoint to register the
passphrase of the user.

#### Request

```http
POST /settings/passphrase HTTP/1.1
Host: alice.example.com
Content-Type: application/json
```

```json
{
    "register_token": "37cddf40d7724988860fa0e03efd30fe",
    "passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791",
    "hint": "a hint to help me remember my passphrase",
    "key": "0.uRcMe+Mc2nmOet4yWx9BwA==|PGQhpYUlTUq/vBEDj1KOHVMlTIH1eecMl0j80+Zu0VRVfFa7X/MWKdVM6OM/NfSZicFEwaLWqpyBlOrBXhR+trkX/dPRnfwJD2B93hnLNGQ=",
    "public_key": "MIIBIjANBgkqhkiG9w...AQAB",
    "private_key": "2.wZuKkufLV31Cpw1v1TQUDA==|u6bUNTaaGxu...y7s=",
    "iterations": 100000
}
```

#### Response

```http
HTTP/1.1 204 No Content
Set-Cookie: cozysessid=AAAAAFhSXT81MWU0ZTBiMzllMmI1OGUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa; Path=/; Domain=alice.example.com; Max-Age=604800; HttpOnly; Secure
```

### POST /settings/passphrase/flagship

This endpoint is similar to `POST /settings/passphrase`, but it allows the
flagship app to also obtain OAuth access and register tokens without having to
make the OAuth dance (which can be awkward for the user).

#### Request

```http
POST /settings/passphrase/flagship HTTP/1.1
Host: alice.example.com
Content-Type: application/json
```

```json
{
    "register_token": "37cddf40d7724988860fa0e03efd30fe",
    "passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791",
    "hint": "a hint to help me remember my passphrase",
    "key": "0.uRcMe+Mc2nmOet4yWx9BwA==|PGQhpYUlTUq/vBEDj1KOHVMlTIH1eecMl0j80+Zu0VRVfFa7X/MWKdVM6OM/NfSZicFEwaLWqpyBlOrBXhR+trkX/dPRnfwJD2B93hnLNGQ=",
    "public_key": "MIIBIjANBgkqhkiG9w...AQAB",
    "private_key": "2.wZuKkufLV31Cpw1v1TQUDA==|u6bUNTaaGxu...y7s=",
    "iterations": 100000,
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


### PUT /settings/passphrase (without two-factor authentication)

The user can change its passphrase with this route.

For users with two-factor authentication activated, a second step on the same
route is necessary to actually update the passphrase. See below.

A `"force": true` parameter can be added in the JSON to force a passphrase on a
Cozy where authentication by password is disabled and the vault is empty. It
allows to use Cozy Pass when the authentication on the Cozy is delegated via
OIDC. When forcing a password reset, you need to regenerate the

* public and private keys
* encryption key

 of the vault and pass them via `key`, `publicKey` and `privateKey`.

See [those password-helpers](https://github.com/cozy/cozy-stack/blob/master/assets/scripts/password-helpers.js#L165-L218) for example on how to regenerate those keys.

#### Request

```http
PUT /settings/passphrase HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Cookie: cozysessid=AAAAAFhSXT81MWU0ZTBiMzllMmI1OGUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa
```

```json
{
    "current_passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791",
    "new_passphrase": "2e7e1e04300356adc8fabf5d304b58c564399746cc7a21464fd6593edd925720",
    "key": "0.uRcMe+Mc2nmOet4yWx9BwA==|PGQhpYUlTUq/vBEDj1KOHVMlTIH1eecMl0j80+Zu0VRVfFa7X/MWKdVM6OM/NfSZicFEwaLWqpyBlOrBXhR+trkX/dPRnfwJD2B93hnLNGQ=",
    "iterations": 10000
}
```

#### Response

```http
HTTP/1.1 204 No Content
Set-Cookie: cozysessid=AAAAShoo3uo1Maic4VibuGohlik2eKUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa; Path=/; Domain=alice.example.com; Max-Age=604800; HttpOnly; Secure
```

### PUT /settings/passphrase (with two-factor authentication)

The user can change its passphrase with this route, with two-factor
authentication to verify the user with more than its passphrase.

#### Request (first step)

```http
PUT /settings/passphrase HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Cookie: cozysessid=AAAAAFhSXT81MWU0ZTBiMzllMmI1OGUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa
```

```json
{
    "current_passphrase": "4f58133ea0f415424d0a856e0d3d2e0cd28e4358fce7e333cb524729796b2791"
}
```

#### Response (first step)

```http
HTTP/1.1 200 OK
Set-Cookie: cozysessid=AAAAShoo3uo1Maic4VibuGohlik2eKUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa; Path=/; Domain=alice.example.com; Max-Age=604800; HttpOnly; Secure
```

```json
{
    "two_factor_token": "YxOSUjxd0SNmuwEEDRHXfw=="
}
```

At this point, the current passphrase has been exchanged against a token, and a
passcode should have been sent to the user to authenticate on the second step.

The token/passcode pair can be used on the second step to update the passphrase.

#### Request (second step)

```http
PUT /settings/passphrase HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Cookie: cozysessid=AAAAAFhSXT81MWU0ZTBiMzllMmI1OGUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa
```

```json
{
    "new_passphrase": "2e7e1e04300356adc8fabf5d304b58c564399746cc7a21464fd6593edd925720",
    "key": "0.uRcMe+Mc2nmOet4yWx9BwA==|PGQhpYUlTUq/vBEDj1KOHVMlTIH1eecMl0j80+Zu0VRVfFa7X/MWKdVM6OM/NfSZicFEwaLWqpyBlOrBXhR+trkX/dPRnfwJD2B93hnLNGQ=",
    "iterations": 10000,
    "two_factor_token": "YxOSUjxd0SNmuwEEDRHXfw==",
    "two_factor_passcode": "4947178"
}
```

#### Response (second step)

```http
HTTP/1.1 204 No Content
Set-Cookie: cozysessid=AAAAShoo3uo1Maic4VibuGohlik2eKUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa; Path=/; Domain=alice.example.com; Max-Age=604800; HttpOnly; Secure
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /settings/passphrase/check

This route can be used to check the passphrase of the user before making
important changes like asking for the Cozy deletion.

#### Request

```http
POST /settings/passphrase/check HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Authentication: Bearer xxx
```

```json
{
    "passphrase": "2e7e1e04300356adc8fabf5d304b58c564399746cc7a21464fd6593edd925720"
}
```

#### Response

It will be `204 No Content` if the passphrase is correct, or a `403 Forbidden` if
the passphrase is incorrect.

### GET /settings/hint

This route can be used to know if a hint has been chosen (but the hint won't be
revealed). If there is a hint, the response will be a `204 No Content`. And, if
not, it will be a `404 No Content`.

#### Request

```http
GET /settings/hint HTTP/1.1
Host: alice.example.com
```

#### Response

```http
HTTP/1.1 204 No Content
```

### PUT /settings/hint

The user can change the hint for its passphrase.

#### Request

```http
PUT /settings/hint HTTP/1.1
Host: alice.example.com
Content-Type: application/json
```

```json
{
  "hint": "My passphrase is very complicated"
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /settings/vault

This route can be used to ensure the vault is initialized. If it is not the
case, it will migrate the accounts from the konnectors accounts to the vault
and will set the `extension_installed` flag.

#### Request

```http
POST /settings/vault HTTP/1.1
Host: alice.example.com
```

#### Response

```http
HTTP/1.1 204 No Content
```

## Instance

### GET /settings/capabilities

List the activated capabilities for this instance. An unadvertised capability
should be considered `false` and, for backward compatibility, if you can't get a
valid response from this endpoint (in particular in case of a `404 Not found`
error), all capabilities should be considered `false`. The current capabilities
are:

- `file_versioning` is true when the VFS can create
  [old versions](https://docs.cozy.io/en/cozy-stack/files/#versions) of a file
- `flat_subdomains` is true when the stack is configured to use flat subdomains
  (not nested)
- `can_auth_with_password` is true when authentication with a Cozy password is
  possible
- `can_auth_with_magic_links` is true when authentication with a link sent by
  email is possible
- `can_auth_with_oidc` is true when delegated authentication with OIDC is
  possible for this instance.
- `resumable_uploads` is true when the files of this instance can be uploaded
  with the [resumable uploads](files.md#resumable-uploads) protocol.

**Note:** both `can_auth_with_password` and `can_auth_with_oidc` can be true
for an instance where the choice is given to the user of how they want to
authenticate.

#### Request

```http
GET /settings/capabilities HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
```

#### Response

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.capabilities",
        "attributes": {
            "file_versioning": true,
            "flat_subdomains": false,
            "can_auth_with_password": true,
            "can_auth_with_magic_links": false,
            "can_auth_with_oidc": false,
            "resumable_uploads": true
        },
        "links": {
            "self": "/settings/capabilities"
        }
    }
}
```

#### Permissions

No permissions are required to access this route, but the request needs to be
authenticated (webapp token, OAuth token, etc.).

### GET /settings/external-ties

List ties between the instance and external services such as a subscription
vendor (e.g. mobile app stores).
The current possible ties are:

- `has_blocking_subscription` is true when the instance is linked to a premium
  subscription paid via a third-party vendor that does not let us cancel the
  subscription ourselves and requires it to be cancelled by the customer
  themselves.

#### Request

```http
GET /settings/external-ties HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
```

#### Response

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.external-ties",
        "attributes": {
            "has_blocking_subscription": false
        },
        "links": {
            "self": "/settings/external-ties"
        }
    }
}
```

#### Permissions

No permissions are required to access this route, but the request needs to be
made from an authenticated Web session or Webapp.

### GET /settings/premium

Redirect to the instance's premium management page if it existes (i.e. the
instance is linked to a manager) without the client knowing about the manager
itself (neither its base URL nor the instance UUID).
If there aren't any manager set up for the given instance, the route returns an
error page with a 404 status code.

#### Request

```http
GET /settings/premium HTTP/1.1
Host: alice.example.com
Accept: text/html
```

#### Permissions

No permissions are required to access this route, but the request needs to be
made from an authenticated Web session or Webapp.

### GET /settings/instance

If the user is logged in, display all instance settings.

#### Request

```http
GET /settings/instance HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
Cookie: sessionid=xxxx
```

#### Response

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.instance",
        "meta": {
            "rev": "3-56521545485448482"
        },
        "attributes": {
            "locale": "fr",
            "password_defined": true,
            "auto_update": true,
            "email": "alice@example.com",
            "public_name": "Alice Martin",
            "auth_mode": "basic",
            "default_redirection": "drive/#/folder",
            "context": "dev",
            "sponsorships": ["springfield"],
            "legal_notice_url": "https://manager.cozycloud.cc/e96388a5-8eed-44cc-81e6-40aad273f0d4.pdf"
        }
    }
}
```

##### Note about `password_defined`

There are a few fields that are persisted on the instance its-self, not on its
settings document. When they are updated, it won't be reflected in the realtime
when listening on the `io.cozy.settings.instance` document.

For `password_defined`, it is possible to be notified when the password is
defined by watching a synthetic document with the doctype `io.cozy.settings`,
and the id `io.cozy.settings.passphrase`.

##### Note about `legal_notice_url`

This attribute will only be present if a manager is associated with the
instance and the instance was created on behalf of a partner with a defined
legal notice.

##### Note about `matrix_id`

This attribute is only present when the `user.created` RabbitMQ message carried
a `matrixId`. It is read-only: like `email`, it is restored from the stored
document on `PUT /settings/instance`, so a client can neither change it nor
erase it with a partial update. When it is absent, the Matrix ID sent to the
common settings API is derived from the email local part and the instance
domain.

### POST /settings/instance/deletion

The settings application can use this route if the user wants to delete their
Cozy instance.

#### Request

```http
POST /settings/instance/deletion HTTP/1.1
Host: alice.example.com
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /settings/instance/moved_from

When there is an attribute `moved_from` in the instance settings, it means that
the Cozy has been moved from the given address. The Home application uses this
information to display a modal to alert the user. And when they dismiss the modal,
a request is made to this route to remove this information (and avoid showing
this modal again the next time they will open the Home again).

#### Request

```http
DELETE /settings/instance/moved_from HTTP/1.1
Host: target.cozy.localhost
Cookie: sessionid=xxxxx
```

#### Response

```http
HTTP/1.1 204 No Content
```

#### Permissions

No permission is required to use this route, only that the user is logged-in.

### PUT /settings/instance

If the user is logged in, allow to set the instance fields

#### Request

```http
POST /settings/instance HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
Content-Type: application/vnd.api+json
Cookie: sessionid=xxxxx
Authorization: Bearer settings-token
```

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.instance",
        "meta": {
            "rev": "3-56521545485448482"
        },
        "attributes": {
            "locale": "fr",
            "email": "alice@example.com",
            "public_name": "Alice Martin",
            "timezone": "Europe/Berlin",
            "auth_mode": "two_factor_mail",
            "default_redirection": "drive/#/folder"
        }
    }
}
```

Note: the format for `default_redirection` is the application slug, followed by
a slash, and then the route for the app (path + fragment).

#### Response

```
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.instance",
        "meta": {
            "rev": "4-5a3e315e"
        },
        "attributes": {
            "locale": "fr",
            "email": "alice@example.com",
            "public_name": "Alice Martin",
            "timezone": "Europe/Berlin",
            "auth_mode": "two_factor_mail",
            "default_redirection": "drive/#/folder"
        }
    }
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.settings` for the verb `PUT`.

### PUT /settings/instance/auth_mode

With this route, the user can ask for the activation of different authentication
modes, like two-factor authentication.

Available authentication modes:

-   `basic`: basic authentication only with passphrase
-   `two_factor_mail`: authentication with passphrase and validation with a code
    sent via email to the user.

When asking for activation of the two-factor authentication, a side-effect can
be triggered to send the user its code (via email for instance), and the
activation not being effective. This side-effect should provide the user with a
code that can be used to finalize the activation of the two-factor
authentication.

Hence, this route has two behaviors:

-   the code is not provided: the route is a side effect to ask for the
    activation of 2FA, and a code is sent
-   the code is provided, and valid: the two-factor authentication is actually
    activated.

Status codes:

-   `204 No Content`: when the mail has been confirmed and two-factor
    authentication is activated
-   `422 Unprocessable Entity`: when the given confirmation code is not good.

#### Request

```http
PUT /settings/instance/auth_mode HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Cookie: cozysessid=AAAAAFhSXT81MWU0ZTBiMzllMmI1OGUyMmZiN2Q0YTYzNDAxN2Y5NjCmp2Ja56hPgHwufpJCBBGJC2mLeJ5LCRrFFkHwaVVa
```

```json
{
    "auth_mode": "two_factor_mail",
    "two_factor_activation_code": "12345678"
}
```

### PUT /settings/instance/sign_tos

With this route, an OAuth client can sign the new TOS version.

Status codes:

-   `204 No Content`: when the mail has been confirmed and two-factor
    authentication is activated

#### Request

```http
PUT /settings/instance/sign_tos HTTP/1.1
Host: alice.example.com
Content-Type: application/json
Authorization: Bearer ...
```

## Sessions

### GET /settings/sessions

This route allows to get all the currently active sessions.

```
GET /settings/sessions HTTP/1.1
Host: cozy.example.org
Cookie: ...
Authorization: Bearer ...
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
    "data": [
        {
            "id": "...",
            "attributes": {
                "last_seen": ""
            },
            "meta": {
                "rev": "..."
            }
        }
    ]
}
```

#### Permissions

This route requires the application to have permissions on the
`io.cozy.sessions` doctype with the `GET` verb.

### GET /settings/sessions/current

This route returns information about the current session.

```
GET /settings/sessions/current HTTP/1.1
Host: cozy.example.org
Cookie: ...
Authorization: Bearer ...
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "data": {
    "id": "...",
    "attributes": {
      "last_seen": "",
      "long_run": true
    },
    "meta": {
      "rev": "..."
    }
  }
}
```

## OAuth 2 clients

### GET /settings/clients

Get the list of the registered clients

#### Request

```http
GET /settings/clients HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
Cookie: sessionid=xxxxx
Authorization: Bearer oauth2-clients-token
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
    "data": [
        {
            "type": "io.cozy.oauth.clients",
            "id": "30e84c10-e6cf-11e6-9bfd-a7106972de51",
            "attributes": {
                "redirect_uris": ["http://localhost:4000/oauth/callback"],
                "client_name": "Cozy-Desktop on my-new-laptop",
                "client_kind": "desktop",
                "client_uri": "https://docs.cozy.io/en/mobile/desktop.html",
                "logo_uri": "https://docs.cozy.io/assets/images/cozy-logo-docs.svg",
                "policy_uri": "https://cozy.io/policy",
                "software_id": "/github.com/cozy-labs/cozy-desktop",
                "software_version": "0.16.0",
                "client_os": "Windows",
                "last_refreshed_at": "2017-09-04T08:14:47Z",
                "synchronized_at": "2017-09-05T16:23:04Z"
            },
            "links": {
                "self": "/settings/clients/30e84c10-e6cf-11e6-9bfd-a7106972de51"
            }
        }
    ]
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.oauth.clients` for the verb `GET` (only client-side apps).

### DELETE /settings/clients/:client-id

#### Request

```http
DELETE /settings/clients/30e84c10-e6cf-11e6-9bfd-a7106972de51 HTTP/1.1
Host: alice.example.com
Authorization: Bearer oauth2-clients-token
```

#### Response

```http
HTTP/1.1 204 No Content
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.oauth.clients` for the verb `DELETE` (only client-side apps).

### GET /settings/clients/limit-exceeded

Get an OAuth clients limit exceeded page if the instance has more connected
OAuth clients than its limit allows or redirect the request to the `redirect`
parameter's value.
The `redirect` parameter is optional. By default, its value
is the instance's default redirection.

The page will auto-refresh every 20 seconds or when an OAuth client deletion is
detected.

#### Query-String

| Parameter  | Description                                          |
| ---------- | ---------------------------------------------------- |
| redirect   | URL where to redirect when the limit is not exceeded |

#### Request

```http
GET /settings/clients/limit-exceeded?redirect=https%3A%2F%2Falice-home.example.com%2F HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
Cookie: sessionid=xxxx
```

### POST /settings/synchronized

Any OAuth2 client can make a request to this endpoint with its token, no
permission is needed. It will update the date of last synchronization for this
device.

#### Request

```http
POST /settings/synchronized HTTP/1.1
Host: alice.example.com
Authorization: Bearer oauth2-access-token
```

#### Response

```http
HTTP/1.1 204 No Content
```

## Avatar

### PUT /settings/avatar

This route can be used to upload the avatar for an instance.

#### Request

```http
PUT /settings/avatar HTTP/1.1
Host: alice.cozy.example.net
Authorization: Bearer token
Content-Type: image/jpeg
...
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /settings/avatar

This route can be used to delete the avatar for an instance.

#### Request

```http
DELETE /settings/avatar HTTP/1.1
Host: alice.cozy.example.net
Authorization: Bearer token
...
```

#### Response

```http
HTTP/1.1 204 No Content
```

## Context

### GET /settings/onboarded

It redirects the user to an application after the onboarding. The application is
selected according to the context of the instance and the configuration of the
stack.

### GET /settings/install_flagship_app

At the end of an onboarding, just after the password has been chosen, if the
user is in a mobile browser, the stack will show a page to push them to install
the flagship app.

### GET /settings/context

It gives the keys/values from the config for the context of the instance.

#### Request

```http
GET /settings/context HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
Cookie: sessionid=xxxx
```

#### Response

```json
{
    "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.context",
        "attributes": {
            "default_redirection": "drive/#/folder",
            "help_link": "https://forum.cozy.io/",
            "onboarded_redirection": "home/#/discovery/?intro"
        },
        "links": {
            "self": "/settings/context"
        }
    }
}
```

#### Permissions

To use this endpoint, an application needs a valid token, but no explicit
permission is required.

## Feature flags

A feature flag is a name and an associated value (boolean, number, string or a
JSON) that can be interpreted by the apps. It can be used for giving access to
paid features, or to enable a feature progressively on all the cozy instances
of a context. The stack computes the feature flags from several sources (in
order of decreasing priority):

- the flags set on the instance by the command `cozy-stack feature flags`
- the response of the manager for `GET /api/v1/flags?sets=s1,s2&context=ctx`
  (where `s1,s2` has been set via `cozy-stack feature sets`)
- the flags coming from the context configuration (`context.<name>.features`)
- the flags set on the context by the command `cozy-stack feature ratio`
- the default value (`cozy-stack feature default`).

For a given flag, the stack takes the value from the source with the highest
priority, and does not look at the other sources (no merge).

### GET /settings/flags

This endpoint returns the computed list of feature flags for the given
instance. It accepts an `include` parameter in the query string to see the
details of how the flags were computed.

#### Request

```http
GET /settings/flags HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.settings",
    "id": "io.cozy.settings.flags",
    "attributes": {
      "has_feature1": true,
      "has_feature2": false,
      "number_of_foos": 10,
      "bar_config": { "qux": "quux" }
    },
    "links": {
      "self": "/settings/flags"
    }
  }
}
```

#### Request

```http
GET /settings/flags?include=source HTTP/1.1
Host: alice.example.com
Accept: application/vnd.api+json
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.settings",
    "id": "io.cozy.settings.flags",
    "attributes": {
      "has_feature1": true,
      "has_feature2": false,
      "number_of_foos": 10,
      "bar_config": { "qux": "quux" }
    },
    "links": {
      "self": "/settings/flags"
    }
  },
  "included": [
    {
      "type": "io.cozy.settings",
      "id": "io.cozy.settings.flags.instance",
      "attributes": {
        "number_of_foos": 10
      }
    },
    {
      "type": "io.cozy.settings",
      "id": "io.cozy.settings.flags.manager",
      "attributes": {
        "sets": ["s1", "s2"],
        "has_feature1": true,
        "number_of_foos": 5
      }
    },
    {
      "type": "io.cozy.settings",
      "id": "io.cozy.settings.flags.config",
      "attributes": {
        "number_of_foos": 2
      }
    },
    {
      "type": "io.cozy.settings",
      "id": "io.cozy.settings.flags.context",
      "attributes": {
        "has_feature2": [{ "ratio": 0.1, "value": true }],
        "bar_config": [
          { "ratio": 0.2, "value": { "qux": "quux" } },
          { "ratio": 0.8, "value": { "qux": "baz" } }
        ]
      }
    },
    {
      "type": "io.cozy.settings",
      "id": "io.cozy.settings.flags.default",
      "attributes": {
        "bar_config": { "qux": "courge" },
        "number_of_foos": 2
      }
    }
  ]
}
```

#### Permissions

To use this endpoint, an application needs a valid token, but no explicit
permission is required.
