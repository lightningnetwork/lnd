Makefile
========

To build, verify, and install `lnd` from source, use the following
commands:
```
make
make check
make install
```

Developers
==========

This document specifies all commands available from `lnd`'s `Makefile`.
The commands included handle:
- Installation of all go-related dependencies.
- Compilation and installation of `lnd` and `lncli`.
- Compilation and installation of `btcd` and `btcctl`.
- Running unit and integration suites.
- Testing, debugging, and flake hunting.
- Formatting and linting.

Commands
========

- [`all`](#scratch)
- [`btcd`](#btcd)
- [`build`](#build)
- [`check`](#check)
- [`clean`](#clean)
- [`default`](#default)
- [`dep`](#dep)
- [`flake-unit`](#flake-unit)
- [`flakehunter`](#flakehunter)
- [`fmt`](#fmt)
- [`install`](#install)
- [`itest`](#itest)
- [`lint`](#lint)
- [`list`](#list)
- [`rpc`](#rpc)
- [`scratch`](#scratch)
- [`travis`](#travis)
- [`unit`](#unit)
- [`unit-cover`](#unit-cover)
- [`unit-race`](#unit-race)

`all`
-----
Compiles, tests, and installs `lnd` and `lncli`. Equivalent to 
[`scratch`](#scratch) [`check`](#check) [`install`](#install).

`btcd`
------
Ensures that [`github.com/Masterminds/glide`][glide] is installed, and
that the [`github.com/btcsuite/btcd`][btcd] repository is checked out
locally. Lastly, installs the version of 
[`github.com/btcsuite/btcd`][btcd] specified in `Gopkg.toml`

`build`
-------
Compiles the current source and vendor trees, creating `./lnd` and
`./lncli`.

`check`
-------
Installs the version of [`github.com/btcsuite/btcd`][btcd] specified
in `Gopkg.toml`, then runs the unit tests followed by the integration
tests.

Related: [`unit`](#unit) [`itest`](#itest)

`clean`
-------
Removes compiled versions of both `./lnd` and `./lncli`, and removes the
`vendor` tree.

`default`
---------
Alias for [`scratch`](#scratch).

`flake-unit`
------------
Runs the unit test endlessly until a failure is detected.

Arguments:
- `pkg=<package>` 
- `case=<testcase>`
- `timeout=<timeout>`

Related: [`unit`](#unit)

`flakehunter`
-------------
Runs the itegration test suite endlessly until a failure is detected.

Arguments:
- `icase=<itestcase>`
- `timeout=<timeout>`

Related: [`itest`](#itest)

`fmt`
-----
Runs `go fmt` on the entire project. 

`install`
---------
Copies the compiled `lnd` and `lncli` binaries into `$GOPATH/bin`.

`itest`
-------
Installs the version of [`github.com/btcsuite/btcd`][btcd] specified in
`Gopkg.toml`, builds the `./lnd` and `./lncli` binaries, then runs the
integration test suite.

Arguments:
- `icase=<itestcase>`
- `timeout=<timeout>`

`lint`
------
Ensures that [`gopkg.in/alecthomas/gometalinter.v1`][gometalinter] is
installed, then lints the project.

`list`
------
Lists all known make targets.

`rpc`
-----
Compiles the `lnrpc` proto files.

`scratch`
---------
Compiles all dependencies and builds the `./lnd` and `./lncli` binaries.
Equivalent to [`lint`](#lint) [`dep`](#dep) [`btcd`](#btcd)
[`unit-race`](#unit-race).

`unit`
------
Runs the unit test suite. By default, this will run all known unit tests.

Arguments:
- `pkg=<package>` 
- `case=<testcase>`
- `timeout=<timeout>`
- `log="stdlog[ <log-level>]"` prints logs to stdout
  - `<log-level>` can be `info` (default), `debug`, `trace`, `warn`, `error`, `critical`, or `off`

`unit-cover`
------------
Runs the unit test suite with test coverage, compiling the statisitics in
`profile.cov`.

Arguments:
- `pkg=<package>` 
- `case=<testcase>`
- `timeout=<timeout>`
- `log="stdlog[ <log-level>]"` prints logs to stdout
  - `<log-level>` can be `info` (default), `debug`, `trace`, `warn`, `error`, `critical`, or `off`

Related: [`unit`](#unit)

`unit-race`
-----------
Runs the unit test suite with go's race detector.

Arguments:
- `pkg=<package>` 
- `case=<testcase>`
- `timeout=<timeout>`
- `log="stdlog[ <log-level>]"` prints logs to stdout
  - `<log-level>` can be `info` (default), `debug`, `trace`, `warn`, `error`, `critical`, or `off`

Related: [`unit`](#unit)

[btcd]: https://github.com/btcsuite/btcd (github.com/btcsuite/btcd")
[glide]: https://github.com/Masterminds/glide (github.com/Masterminds/glide)
[gometalinter]: https://gopkg.in/alecthomas/gometalinter.v1 (gopkg.in/alecthomas/gometalinter.v1)
[dep]: https://github.com/golang/dep/cmd/dep (github.com/golang/dep/cmd/dep)
[goveralls]: https://github.com/mattn/goveralls (github.com/mattn/goveralls)
