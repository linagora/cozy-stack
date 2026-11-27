[Table of contents](README.md#table-of-contents)

# Jobs

Jobs are designed to represent asynchronous tasks that your cozy can execute.
These tasks can be scheduled in advance, recurring or sudden and provide various
services.

At the time, we do not provide "generic" and programmable tasks. This list of
available workers is part of the defined API.

The job queue can be either local to the cozy-stack when used as a monolithic
instance (self-hosted for instance) or distributed via a redis server for
distributed infrastructures.

This doc introduces two cozy types:

- `io.cozy.jobs` for jobs
- `io.cozy.triggers` for triggers

## Triggers

Jobs can be queued up via the `/jobs/queue/:worker-type` API, for a direct
execution. But it can also be convenient to schedule jobs on some conditions,
and the triggers are the way to do that.

Jobs can be launched by different types of triggers:

- `@at` to schedule a one-time job executed after at a specific time in the
  future
- `@in` to schedule a one-time job executed after a specific amount of time
- `@hourly` to schedule jobs that run once an hour
- `@daily` to schedule jobs that run once a day
- `@weekly` to schedule jobs that run once a week
- `@monthly` to schedule jobs that run once a month
- `@every` to schedule periodic jobs executed at a given fix interval
- `@cron` to schedule recurring jobs scheduled at specific times
- `@event` to launch a job after a change on documents in the cozy
- `@webhook` to launch a job when an HTTP request hit a specific URL
- `@client` when the client controls when the job are launched.

These triggers have specific syntaxes to describe when jobs should be
scheduled. See below for more informations.

### `@at` syntax

The `@at` trigger takes a ISO-8601 formatted string indicating a UTC time in the
future. The date is of this form: `YYYY-MM-DDTHH:mm:ss.sssZ`

:warning: Be aware that the `@at` trigger is removed from the doctype after it has created the associated job.

Examples

```
@at 2018-12-12T15:36:25.507Z
```

### `@in` syntax

The `@in` trigger takes the same duration syntax as `@every`

:warning: Be aware that the `@in` trigger is removed from the doctype after it has created the associated job.

Examples

```
@in 10m
@in 1h30m
```

### `@daily` syntax

The `@daily` trigger will create a job once a day. By default, the stack is
free to choose the day and hour when to do that, but you can add restrictions:

```
@daily                      # Once a day, any hour
@daily before 5am           # Once a day, between midnight and 5am (UTC)
@daily after 10pm           # Once a day, between 10pm and midnight (UTC)
@daily between 8am and 6pm  # Once a day, between 8am and 6pm (UTC)
```

### `@weekly` syntax

The `@weekly` trigger will create a job once a week. By default, the stack is
free to choose the day and hour when to do that, but you can add restrictions:

```
@weekly                      # Once a week, any day, any hour
@weekly on monday            # Every monday
@weekly on mon,wed,fri       # Once a week, on a monday, wednesday, or friday
@weekly on wed-fri           # Once a week, on a wednesday, thursday, or friday
@weekly on weekday           # Once a week, but not the week-end
@weekly on weekend           # Every week-end (saturday or sunday)
@weekly before 5am           # Once a week, any day, but between midnight and 5am (UTC)
@weekly after 10pm           # Once a week, any day, but between 10pm and midnight (UTC)
@weekly between 8am and 6pm  # Once a week, any day, between 8am and 6pm (UTC)
@weekly on monday before 9am # Every monday, between midnight and 9am (UTC)
```

### `@monthly` syntax

The `@monthly` trigger will create a job once a month. By default, the stack is
free to choose the day and hour when to do that, but you can add restrictions:

```
@monthly                      # Once a month, any day, any hour
@monthly on the 1             # Every first day of the month
@monthly on the 1-5           # Once a month, on the first five days of the month
@monthly before 5am           # Once a month, any day, but between midnight and 5am (UTC)
@monthly after 10pm           # Once a month, any day, but between 10pm and midnight (UTC)
@monthly between 8am and 6pm  # Once a month, any day, between 8am and 6pm (UTC)
@monthly on the 1 before 9am  # Every first day of the month, between midnight and 9am (UTC)
```

**Note:** the current implementation is to take a random day/hour and run the
job each month at this day/hour. So, you should avoid 29-31 if you really want
the job to run each month.

### `@every` syntax

The `@every` trigger uses the same syntax as golang's `time.ParseDuration` (but
only support time units above seconds):

A duration string is a possibly signed sequence of decimal numbers, each with
optional fraction and a unit suffix, such as "1.5h" or "2h45m". Valid time
units are "s", "m", "h".

A trigger created with `POST /jobs/triggers` must have an interval of at least
one minute: a shorter one is refused with a `400 Bad Request`.

Examples

```
@every 1.5h   # schedules every 1 and a half hours
@every 30m10s # schedules every 30 minutes and 10 seconds
```

### `@cron` syntax

In order to schedule recurring jobs, the `@cron` trigger has the syntax using
six fields:

| Field name   | Mandatory? | Allowed values  | Allowed special characters |
| ------------ | ---------- | --------------- | -------------------------- |
| Seconds      | Yes        | 0-59            | \* / , -                   |
| Minutes      | Yes        | 0-59            | \* / , -                   |
| Hours        | Yes        | 0-23            | \* / , -                   |
| Day of month | Yes        | 1-31            | \* / , - ?                 |
| Month        | Yes        | 1-12 or JAN-DEC | \* / , -                   |
| Day of week  | Yes        | 0-6 or SUN-SAT  | \* / , - ?                 |

Asterisk ( `*` )

The asterisk indicates that the cron expression will match for all values of the
field; e.g., using an asterisk in the 5th field (month) would indicate every
month.

Slash ( `/` )

Slashes are used to describe increments of ranges. For example 3-59/15 in the
1st field (minutes) would indicate the 3rd minute of the hour and every 15
minutes thereafter. The form `"*\/..."` is equivalent to the form
`"first-last/..."`, that is, an increment over the largest possible range of the
field. The form `"N-/..."` is accepted as meaning `"N-MAX/..."`, that is,
starting at N, use the increment until the end of that specific range. It does
not wrap around.

Comma ( `,` )

Commas are used to separate items of a list. For example, using `"MON,WED,FRI"`
in the 5th field (day of week) would mean Mondays, Wednesdays and Fridays.

Hyphen ( `-` )

Hyphens are used to define ranges. For example, 9-17 would indicate every hour
between 9am and 5pm inclusive.

Question mark ( `?` )

Question mark may be used instead of `*` for leaving either day-of-month or
day-of-week blank.

To schedule jobs given an interval:

Examples:

```
@cron 0 0 0 1 1 *  # Run once a year, midnight, Jan. 1st
@cron 0 0 0 1 1 *  # Run once a year, midnight, Jan. 1st
@cron 0 0 0 1 * *  # Run once a month, midnight, first of month
@cron 0 0 0 * * 0  # Run once a week, midnight on Sunday
@cron 0 0 0 * * *  # Run once a day, midnight
@cron 0 0 * * * *  # Run once an hour, beginning of hour
```

### `@event` syntax

The `@event` syntax allows to trigger a job when something occurs in the stack.
It follows the same syntax than [permissions
scope](https://docs.cozy.io/en/cozy-stack/permissions/#what-is-a-permission)
string:

`type[:verb][:values][:selector]`

Unlike for permissions string, the verb should be one of `CREATED`, `DELETED`,
`UPDATED`. It is possible to put several verbs, separated by a comma.

There is also a special value `!=`. It means that a job will be trigger only if
the value for the given selector has changed (ie the value before the update and
the value after that are different).

The job worker will receive a compound message including original trigger_infos
messages and the event which has triggered it.

Examples:

```
@event io.cozy.files // anything happens on files
@event io.cozy.files:CREATED // a file was created
@event io.cozy.files:DELETED:image/jpg:mime // an image was deleted
@event io.cozy.bank.operations:CREATED io.cozy.bank.bills:CREATED // a bank operation or a bill
@event io.cozy.bank.operations:CREATED,UPDATED // a bank operation created or updated
@event io.cozy.bank.operations:UPDATED:!=:category // a change of category for a bank operation
```

### `@webhook` syntax

It takes no parameter. The URL to hit is not controlled by the request, but is
chosen by the server (and is returned as `webhook` in the `links` JSON-API
response).

Example:

```
@webhook
```

### `@client` syntax

It takes no parameter and can only by used for the `client` worker. The stack
won't create a job unless a client calls the launch endpoint for this trigger.
The main goal of this trigger is keep a state, as the aggregation of job
results.

## Error Handling

Jobs can fail to execute their task. We have two ways to parameterize such
cases.

### Retry

A retry count can be optionally specified to ask the worker to re-execute the
task if it has failed.

Each retry is executed after a configurable delay. The try count is part of the
attributes of the job. Also, each occurring error is kept in the `errors` field
containing all the errors that may have happened.

### Timeout

A worker may never end. To prevent this, a configurable timeout value is
specified with the job.

If a job does not end after the specified amount of time, it will be aborted. A
timeout is just like another error from the worker and can provoke a retry if
specified.

### Defaults

By default, jobs are parameterized with a maximum of 3 tries with 1 minute
timeout.

These defaults may vary given the workload of the workers.

## Jobs API

Example and description of the attributes of a `io.cozy.jobs`:

```js
{
  "domain": "me.cozy.localhost",
  "worker": "sendmail",    // worker type name
  "options": {
    "timeout": 60,         // timeout value in seconds
    "max_exec_count": 3,   // maximum number of time the job should be executed (including retries)
  },
  "arguments": {           // the arguments will be given to the worker (if you look in CouchDB, it is called message there)
    "mode": "noreply",
    "template_name": "new_registration",
    "template_values": {
      "DevicesLink": "http://me.cozy.localhost/#/connectedDevices",
    }
  },
  "state": "running",      // queued, running, done, errored
  "queued_at": "2016-09-19T12:35:08Z",  // time of the queuing
  "started_at": "2016-09-19T12:35:08Z", // time of first execution
  "error": ""             // error message if any
}
```

Example and description of a job creation options — as you can see, the options
are replicated in the `io.cozy.jobs` attributes:

```js
{
  "timeout": 60,         // timeout value in seconds
  "max_exec_count": 3,   // maximum number of retry
}
```

### GET /jobs/:job-id

Get a job informations given its ID.

#### Request

```http
GET /jobs/123123 HTTP/1.1
Accept: application/vnd.api+json
```

#### Response

```json
{
  "data": {
    "type": "io.cozy.jobs",
    "id": "123123",
    "attributes": {
      "domain": "me.cozy.localhost",
      "worker": "sendmail",
      "options": {
        "timeout": 60,
        "max_exec_count": 3
      },
      "state": "running",
      "queued_at": "2016-09-19T12:35:08Z",
      "started_at": "2016-09-19T12:35:08Z",
      "error": ""
    },
    "links": {
      "self": "/jobs/123123"
    }
  }
}
```

### POST /jobs/queue/:worker-type

Enqueue programmatically a new job.

This route requires a specific permission on the worker-type. A global
permission on the global `io.cozy.jobs` doctype is not allowed.

Each [worker](./workers.md) accepts different arguments. For konnectors, the
arguments will be given in the `process.env['COZY_FIELDS']` variable.

#### Request

```http
POST /jobs/queue/sendmail HTTP/1.1
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
  "data": {
    "attributes": {
      "manual": false,
      "options": {
        "timeout": 60,
        "max_exec_count": 3
      },
      "arguments": {} // any json value used as arguments for the job
    }
  }
}
```

#### Response

```json
{
  "data": {
    "type": "io.cozy.jobs",
    "id": "123123",
    "attributes": {
      "domain": "me.cozy.localhost",
      "worker": "sendmail",
      "options": {
        "timeout": 60,
        "max_exec_count": 3
      },
      "state": "running",
      "queued_at": "2016-09-19T12:35:08Z",
      "started_at": "2016-09-19T12:35:08Z",
      "error": ""
    },
    "links": {
      "self": "/jobs/123123"
    }
  }
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.jobs` for the verb `POST`. The is required to restrict its permission
to specific worker(s), like this (a global permission on the doctype is not
allowed):

```json
{
  "permissions": {
    "mail-from-the-user": {
      "description": "Required to send mails from the user to his/her friends",
      "type": "io.cozy.jobs",
      "verbs": ["POST"],
      "selector": "worker",
      "values": ["sendmail"]
    }
  }
}
```

### POST /jobs/support

Send a mail to the support (email address defined by `mail.reply_to` in the
config file, or overwritten by context with `contexts.<name>.reply_to`).

It requires a permission on `io.cozy.support` (a permission on
`io.cozy.jobs:POST:sendmail:worker`, or larger, is also accepted to ease the
transition from sending manually a mail to the support via the sendmail queue).

#### Request

```http
POST /jobs/support HTTP/1.1
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "attributes": {
      "arguments": {
        "subject": "Cozy is so cool!",
        "body": "I really love Cozy. Thank you so much!"
      }
    }
  }
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /jobs/campaign-emails

Send a non transactional (or campaign) email to the user via the dedicated
campaign mail server (configured via `campaign_mail` attributes in the config
file, or overwritten by context with `campaign_mail.contexts.<name>`
attributes).

Both the subject and at least one part are required.

#### Request

```http
POST /jobs/campaign-emails HTTP/1.1
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "attributes": {
      "arguments": {
        "subject": "Checkout the new cool stuff!",
        "parts": [
          {
            "body": "So many new features to check out!",
            "type": "text/plain"
          }
        ]
      }
    }
  }
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.jobs` for the verb `POST` and the `sendmail` worker.
This can be defined like so:

```json
{
  "permissions": {
    "campaign-emails": {
      "description": "Required to send campaign emails to the user",
      "type": "io.cozy.jobs",
      "verbs": ["POST"],
      "selector": "worker",
      "values": ["sendmail"]
    }
  }
}
```

### GET /jobs/queue/:worker-type

List the jobs in the queue.

#### Request

```http
GET /jobs/queue/sendmail HTTP/1.1
Accept: application/vnd.api+json
```

#### Response

```json
{
  "data": [
    {
      "attributes": {
        "domain": "cozy.localhost:8080",
        "options": null,
        "queued_at": "2017-09-29T15:32:31.953878568+02:00",
        "started_at": "0001-01-01T00:00:00Z",
        "state": "queued",
        "worker": "log"
      },
      "id": "77689bca9634b4fb08d6ca3d1643de5f",
      "links": {
        "self": "/jobs/log/77689bca9634b4fb08d6ca3d1643de5f"
      },
      "meta": {
        "rev": "1-f823bcd2759103a5ad1a98f4bf083b36"
      },
      "type": "io.cozy.jobs"
    }
  ],
  "meta": {
    "count": 0
  }
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.jobs` for the verb `GET`. In most cases, the application will restrict
its permission to only one worker, like this:

```json
{
  "permissions": {
    "mail-from-the-user": {
      "description": "Required to know the number of jobs in the sendmail queues",
      "type": "io.cozy.jobs",
      "verbs": ["GET"],
      "selector": "worker",
      "values": ["sendmail"]
    }
  }
}
```

### PATCH /jobs/:job-id

This endpoint can be used for a job of the `client` worker (executed by a
client, not on the server) to update the status.

#### Request

```http
PATCH /jobs/022368c07dc701396403543d7eb8149c HTTP/1.1
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.jobs",
    "id": "022368c07dc701396403543d7eb8149c",
    "attributes": {
      "state": "errored",
      "error": "LOGIN_FAILED"
    }
  }
}
```

### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.jobs",
    "id": "022368c07dc701396403543d7eb8149c",
    "attributes": {
      "domain": "me.cozy.localhost",
      "worker": "sendmail",
      "options": {},
      "state": "errored",
      "error": "LOGIN_FAILED",
      "queued_at": "2021-04-12T12:34:56Z",
      "started_at": "2021-04-12T12:34:56Z",
      "finished_at": "2021-04-12T12:38:59Z"
    },
    "links": {
      "self": "/jobs/022368c07dc701396403543d7eb8149c"
    }
  }
}
```

### POST /jobs/triggers

Add a trigger of the worker. See [triggers' descriptions](#triggers) to see the
types of trigger and their arguments syntax.

This route requires a specific permission on the worker type. A global
permission on the global `io.cozy.triggers` doctype is not allowed.

The `debounce` parameter can be used to limit the number of jobs created in a
burst. It delays the creation of the job on the first input by the given time
argument, and if the trigger has its condition matched again during this period,
it won't create another job. It can be useful to combine it with the changes
feed of couchdb with a last sequence number persisted by the worker, as it
allows to have a nice diff between two executions of the worker. Its syntax is the
one understood by go's [time.ParseDuration](https://golang.org/pkg/time/#ParseDuration).

#### Request

```http
POST /jobs/triggers HTTP/1.1
Accept: application/vnd.api+json
```

```json
{
  "data": {
    "attributes": {
      "type": "@event",
      "arguments": "io.cozy.invitations",
      "debounce": "10m",
      "worker": "sendmail",
      "message": {},
      "options": {
        "timeout": 60,
        "max_exec_count": 3
      }
    }
  }
}
```

**Note**: the `message` field was previously called `worker_arguments`. The
latter version still works but is deprecated, you should use `message` instead.

#### Response

```json
{
  "data": {
    "type": "io.cozy.triggers",
    "id": "123123",
    "attributes": {
      "type": "@every",
      "arguments": "30m10s",
      "debounce": "10m",
      "worker": "sendmail",
      "options": {
        "timeout": 60,
        "max_exec_count": 3
      }
    },
    "links": {
      "self": "/jobs/triggers/123123"
    }
  }
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.triggers` for the verb `POST`. In most cases, the application will
restrict its permission to only one worker, like this:

```json
{
  "permissions": {
    "mail-from-the-user": {
      "description": "Required to send regularly mails from the user to his/her friends",
      "type": "io.cozy.triggers",
      "verbs": ["POST"],
      "selector": "worker",
      "values": ["sendmail"]
    }
  }
}
```

### GET /jobs/triggers/:trigger-id

Get a trigger informations given its ID.

#### Request

```http
GET /jobs/triggers/123123 HTTP/1.1
Accept: application/vnd.api+json
```

#### Response

```json
{
  "data": {
    "type": "io.cozy.triggers",
    "id": "123123",
    "attributes": {
      "type": "@every",
      "arguments": "30m10s",
      "worker": "sendmail",
      "options": {
        "timeout": 60,
        "max_exec_count": 3
      },
      "current_state": {
        "status": "done",
        "last_success": "2017-11-20T13:31:09.01641731",
        "last_successful_job_id": "abcde",
        "last_execution": "2017-11-20T13:31:09.01641731",
        "last_executed_job_id": "abcde",
        "last_failure": "2017-11-20T13:31:09.01641731",
        "last_failed_job_id": "abcde",
        "last_error": "error value",
        "last_manual_execution": "2017-11-20T13:31:09.01641731",
        "last_manual_job_id": "abcde"
      }
    },
    "links": {
      "self": "/jobs/triggers/123123"
    }
  }
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.triggers` for the verb `GET`. A konnector can also call this endpoint
for one of its triggers (no permission required).

### GET /jobs/triggers/:trigger-id/state

Get the trigger current state, to give a big picture of the health of the
trigger.

- last executed job status (`done`, `errored`, `queued` or `running`)
- last executed job that resulted in a successful executoin
- last executed job that resulted in an error
- last executed job from a manual execution (not executed by the trigger
  directly)

#### Request

```
GET /jobs/triggers/123123/state HTTP/1.1
Accept: application/vnd.api+json
```

#### Response

```json
{
  "data": {
    "type": "io.cozy.triggers.state",
    "id": "123123",
    "attributes": {
      "status": "done",
      "last_success": "2017-11-20T13:31:09.01641731",
      "last_successful_job_id": "abcde",
      "last_execution": "2017-11-20T13:31:09.01641731",
      "last_executed_job_id": "abcde",
      "last_failure": "2017-11-20T13:31:09.01641731",
      "last_failed_job_id": "abcde",
      "last_error": "error value",
      "last_manual_execution": "2017-11-20T13:31:09.01641731",
      "last_manual_job_id": "abcde"
    }
  }
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.triggers` for the verb `GET`. A konnector can also call this endpoint
for one of its triggers (no permission required).

### GET /jobs/triggers/:trigger-id/jobs

Get the jobs launched by the trigger with the specified ID.

Query parameters:

- `Limit`: to specify the number of jobs to get out

#### Request

```http
GET /jobs/triggers/123123/jobs?Limit=1 HTTP/1.1
Accept: application/vnd.api+json
```

#### Response

```json
{
  "data": [
    {
      "type": "io.cozy.jobs",
      "id": "123123",
      "attributes": {},
      "links": {
        "self": "/jobs/123123"
      }
    }
  ]
}
```

### PATCH /jobs/triggers/:trigger-id

This route can be used to change the frequency of execution of a `@cron`
trigger, or the message of any trigger.

#### Request

```http
PATCH /jobs/triggers/123123 HTTP/1.1
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
  "data": {
    "attributes": {
      "type": "@cron",
      "arguments": "0 0 0 * * 0"
    }
  }
}
```

#### Response

```json
{
  "data": {
    "type": "io.cozy.triggers",
    "id": "123123",
    "attributes": {
      "type": "@cron",
      "arguments": "0 0 0 * * 0",
      "worker": "sendmail",
      "options": {
        "timeout": 60,
        "max_exec_count": 3
      }
    },
    "links": {
      "self": "/jobs/triggers/123123"
    }
  }
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.triggers` for the verb `PATCH`. A konnector can also call this
endpoint for one of its triggers (no permission required).

### POST /jobs/triggers/:trigger-id/launch

Launch a trigger manually given its ID and return the created job.

**Note:** this endpoint can be used to create a job for a `@client` trigger. In
that case, the job won't be executed on the server but by the client. And the client
must call `PATCH /jobs/:job-id` when the job is completed (success or error).

#### Request

```http
POST /jobs/triggers/123123/launch HTTP/1.1
Accept: application/vnd.api+json
```

#### Response

```json
{
  "data": {
    "type": "io.cozy.jobs",
    "id": "123123",
    "attributes": {
      "domain": "me.cozy.localhost",
      "worker": "sendmail",
      "options": {},
      "state": "running",
      "queued_at": "2016-09-19T12:35:08Z",
      "started_at": "2016-09-19T12:35:08Z",
      "error": ""
    },
    "links": {
      "self": "/jobs/123123"
    }
  }
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.triggers` for the verb `POST`.

### DELETE /jobs/triggers/:trigger-id

Delete a trigger given its ID.

#### Request

```http
DELETE /jobs/triggers/123123 HTTP/1.1
Accept: application/vnd.api+json
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.triggers` for the verb `DELETE`. A konnector can also call this
endpoint for one of its triggers (no permission required).

### GET /jobs/triggers

Get the list of triggers. This route only accept `Worker` and `Type` query
parameters and returns the trigger but also in `attributes` its `current_state`
(the same `current_state` returned by [GET
/jobs/triggers/:trigger-id](jobs/#get-jobstriggerstrigger-id)). Be warned that
`/data/io.cozy.triggers/_find` does not return this `current_state` attribute
and you'll need to query `/jobs/triggers/:trigger-id` to have it.

Query parameters (with comma-separated values):

- `Type`: to filter on the trigger type (`@cron`, `@in`, etc.)
- `Worker`: to filter only triggers associated with a specific worker.

#### Request

```http
GET /jobs/triggers?Worker=konnector&Type=@cron,@in,@at HTTP/1.1
Accept: application/vnd.api+json
```

#### Response

```json
{
  "data": [
    {
      "type": "io.cozy.triggers",
      "id": "123123",
      "arguments": "0 40 0 * * *",
      "current_state": {
        "last_error": "LOGIN_FAILED",
        "last_executed_job_id": "abc",
        "last_execution": "2019-01-07T08:23:22.069902888Z",
        "last_failed_job_id": "abcd",
        "last_failure": "2019-01-07T08:23:22.069902888Z",
        "last_manual_execution": "2019-01-07T08:23:22.069902888Z",
        "last_manual_job_id": "azer",
        "status": "errored",
        "trigger_id": "123123"
      },
      "debounce": "",
      "domain": "xxx.mycozy.cloud",
      "message": {
        "konnector": "slug",
        "account": "XXX"
      },
      "options": null,
      "type": "@cron",
      "worker": "konnector",
      "id": "123123",
      "links": {
        "self": "/jobs/triggers/123123"
      }
    }
  ]
}
```

#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.triggers` for the verb `GET`. When used on a specific worker, the
permission can be specified on the `worker` field.

### POST /jobs/webhooks/:trigger-id

This endpoint is used for creating a job (for example executing a konnector
or a service).
It requires no permission, but a trigger of type `@webhook` must have been
created before using this endpoint. Its body must be a JSON that will be
available to the konnector or to the service through the
`process.env['COZY_PAYLOAD']` variable.

It is possible to pass a `Manual=true` parameter in the query-string if the job
is interactive. It will give it an higher priority in the queues.

#### Request

```http
POST /jobs/webhooks/f34c74d0-0c91-0139-5af5-543d7eb8149c HTTP/1.1
Content-Type: application/json
```

```json
{
  "account_id": "0672e560"
}
```

#### Response

```
HTTP/1.1 204 No Content
```

### POST /jobs/webhooks/bi

This endpoint is used to create jobs for banking konnectors. It requires a
payload with the format defined by [Budget
Insight](https://docs.budget-insight.com/guides/webhooks) and an
`Authorization` header with a Bearer token, where a trigger and an account can
be found on this instance matching their data. The event type and the URL of
the BI API (on the good environment) are also sent in the query string.

#### Request

```http
POST /jobs/webhooks/bi?event=CONNECTION_SYNCED&bi_url=https://.../ HTTP/1.1
Content-Type: application/json
Authorization: Bearer token-from-bi
Host: cozy.example.com
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /jobs/purge

This endpoint allows to purge old jobs of an instance.
Some parameters can be given to this route:

* `duration` is the duration of jobs to keep. This is a human-readable string
  (integer+suffix).
  For example:
  - "3W" will keep jobs up to 3 weeks
  - "2M" will keep jobs up to 2 months
* `workers` is a comma-separated list of workers to apply the purge job.

#### Request

```http
DELETE /jobs/purge HTTP/1.1
Accept: application/json
```

#### Response

```json
{
  "deleted": 42
}
```
#### Permissions

To use this endpoint, an application needs a permission on the type
`io.cozy.jobs` for the verb `DELETE`.

## Worker pool

The consuming side of the job queue is handled by a worker pool.

On a monolithic cozy-stack, the worker pool has a configurable fixed size of
workers. The default value is not yet determined. Each time a worker has
finished a job, it check the queue and based on the priority and the queued date
of the job, picks a new job to execute.

## Permissions

In order to prevent jobs from leaking informations between applications, we may
need to add filtering per applications: for instance one queue per applications.

We should also have an explicit check on the permissions of the applications
before launching a job scheduled by an application. For more information, refer
to our [permission document](../permissions).

## Multi-stack

When some instances are served by several stacks, the scheduling and running of
jobs can be distributed on the stacks. The synchronization is done via redis.

For scheduling, there is one important key in redis: `triggers`. It's a sorted
set. The members are the identifiants of the triggers (with the domain name),
and the score are the timestamp of the next time the trigger should occur.
During the short period of time where a trigger is processed, its key is moved
to `scheduling` (another sorted set). So, even if a stack crash during
processing a trigger, this trigger won't be lost.

For `@event` triggers, we don't use the same mechanism. Each stack has all the
triggers in memory and is responsible to trigger them for the events generated
by the HTTP requests of their API. They also publish them on redis: this pub/sub
is used for the realtime API.
