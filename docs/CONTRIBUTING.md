# How to contribute to the Cozy Stack?

Thank you for your interest in contributing to Cozy! There are many ways to
contribute, and we appreciate all of them.

## Security Issues

If you discover a security issue, please bring it to their attention right away!
Please **DO NOT** file a public issue, instead send your report privately to
security AT cozycloud DOT cc.

Security reports are greatly appreciated and we will publicly thank you for it.
We currently do not offer a paid security bounty program, but are not ruling it
out in the future.

## Bug Reports

While bugs are unfortunate, they're a reality in software. We can't fix what we
don't know about, so please report liberally. If you're not sure if something is
a bug or not, feel free to file a bug anyway.

Opening an issue is as easy as following
[this link](https://github.com/cozy/cozy-stack/issues/new) and filling out the
fields. Here are some things you can write about your bug:

-   A short summary
-   What did you try, step by step?
-   What did you expect?
-   What did happen instead?
-   What is the version of the Cozy Stack?

You can also use the [`cozy-stack tools bug`](cli/cozy-stack_tools_bug.md) command to
open the form to report issue prefilled with some useful system informations.

## Pull Requests

### Workflow

Pull requests are the primary mechanism we use to change Cozy. GitHub itself has
some
[great documentation ](https://help.github.com/categories/collaborating-with-issues-and-pull-requests/)
on using the Pull Request feature. We use the 'fork and pull' model described
there.

#### Step 1: Fork

Fork the project on GitHub and check out your copy locally:

```
$ git clone https://github.com/cozy/cozy-stack.git
$ cd cozy-stack
$ git remote add fork git://github.com/username/cozy-stack.git
```

#### Step 2: Branch

Create a branch and start hacking:

```
$ git checkout -b my-branch -t origin/master
```

#### Step 3: Code


Well, I think you know how to do that. Just be sure to follow the coding
guidelines from the Go community (gofmt,
[Effective Go](https://golang.org/doc/effective_go.html), comment the code,
etc.).

We are using [goimports](https://godoc.org/golang.org/x/tools/cmd/goimports) to
format code, and [golangci-lint](https://github.com/golangci/golangci-lint) to
detect code smells.

```
$ make lint
```

We are using [eslint](https://eslint.org/) to lint JavaScript code. The linting
rules are based on
[cozy-app](https://github.com/cozy/cozy-libs/tree/master/packages/eslint-config-cozy-app)

```
$ make jslint
```

#### Step 4: Test

See the [testing architecture](testing.md) for test layers, load-test boundaries
and the shared DEV rollout. `make unit-tests` uses `-short`; CI also runs Go
integration tests with their dependencies.

Don't forget to add tests and be sure they are green. You need CouchDB
installed, with an account, and configuring the stack to use this account:

```
$ export COZY_COUCHDB_URL=http://admin:password@localhost:5984
$ make unit-tests
```

If you want to play with the modified cozy-stack (for example, testing it with a
webapp), you can build it locally and start it with this command:

```
$ make run
```

#### Step 5: Commit

Writing
[good commit messages](http://tbaggery.com/2008/04/19/a-note-about-git-commit-messages.html)
is important. A commit message should describe what changed and why.

#### Step 6: Rebase

Use `git pull --rebase`, or `git rebase` (but not `git merge`), to sync your
work from time to time:

```
$ git pull origin master --rebase
```

#### Step 7: Push

```
$ git push fork my-branch
```

Go to https://github.com/username/cozy-stack and select your branch. Click the
'Pull Request' button and fill out the form.

Pull requests are usually reviewed within a few days. If there are comments to
address, apply your changes in a separate commit and push that to your branch.

### AI-assisted review

The [PR review bot and prompts](../.github/review/README.md) live in this repository.
Once configured, the bot reviews new commits and updates its GitHub comments.
People with write access can request another run by commenting `/review`.
Humans decide what to fix, resolve discussions and approve or merge.

## Code organization

The codebase of cozy-stack contains several packages, and it is quite easy to
have circular import issues in go. To limit the risk, we have split the
packages in several directories with some rules for the imports. In short,
a package in this list should import other packages that are on the same line
or below:

- `main` and `cmd` are the top level packages
- `web` is where we have the routers and handlers for web requests
- `worker` is where we define the workers for our job system
- `model` is for high-level internal packages (in general one package is used
  for one doctype)
- `client` is a small number of packages used for writing clients for the stack
- `pkg` is the low-level packages (most of those packages are just a couple of
  structs and functions).

Note that `tests/testutils` can be used safely in `web` and `worker` packages.
In `model`, it can be used but it is recommended to use a fake package for the
tests if it is the case. For example, `model/oauth/client_test.go` is declared
as `package oauth_test`.

## External assets

The cozy-stack serve some assets for the client application. In particular,
cozy-client-js and cozy-bar assets are listed in `assets/.externals`. To update
them, you can open a pull request for this file. When a maintainer will accept
this pull request, he will also run `make assets` to transform them in go code
(to make the repository go gettable).

## Useful commands

There are some useful commands to know in order to develop with the go code of
cozy-stack:

```bash
cd cozy-stack
make help               # Show the commands that can be launched via make
go build                # Build the stack, also takes care of updating dependencies through gomodules
go install              # Installs the `cozy-stack` binary
go test -v ./...        # To launch the tests
go run main.go serve    # To start the API server
godoc -http=:6060       # To start the documentation server
                        # Open http://127.0.0.1:6060/pkg/github.com/cozy/cozy-stack/
```

## Writing documentation

Documentation improvements are very welcome. We try to keep a good documentation
in the `docs/` folder. But, you know, we are developers, we can forget to
document important stuff that look obvious to us. And documentation can always
be improved.

## Translations

The Cozy Stack is translated on a platform called
[Transifex](https://www.transifex.com/cozy/).
[This tutorial](https://docs.transifex.com/getting-started-1/translators) can help
you to learn how to make your first steps here. If you have any question, don't
hesitate to ask us!

The translations are kept synchronized with transifex via their github integration.

## Community

You can help us by making our community even more vibrant. For example, you can
write a blog post, take some videos, answer the questions on
[the forum](https://forum.cozycloud.cc), organize new meetups, and speak about
what you like in Cozy!
