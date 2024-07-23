Makefile
========

To build, verify, and install `lnd` from source, use the following
commands:
```shell
$  make
$  make check
$  make install
```

The command `make check` requires `bitcoind` (almost any version should do) to
be available in the system's `$PATH` variable. Otherwise, some tests will
fail.

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
Ensures that the [`github.com/btcsuite/btcd`][btcd] repository is checked out
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
Runs the integration test suite endlessly until a failure is detected.

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
- `icase=<itestcase>` (the snake_case version of the testcase name field in the testCases slice (i.e. sweep_coins), not the test func name)
- `timeout=<timeout>`

`itest-parallel`
------
Does the same as `itest` but splits the total set of tests into
`NUM_ITEST_TRANCHES` tranches (currently set to 6 by default, can be overwritten
by setting `tranches=Y`) and runs them in parallel.

Arguments:
- `icase=<itestcase>`: The snake_case version of the testcase name field in the
  testCases slice (i.e. `sweep_coins`, not the test func name) or any regular
  expression describing a set of tests.
- `timeout=<timeout>`
- `tranches=<number_of_tranches>`: The number of parts/tranches to split the
  total set of tests into.
- `parallel=<number_of_threads>`: The number of threads to run in parallel. Must
  be greater or equal to `tranches`, otherwise undefined behavior is expected.

`flakehunter-parallel`
------
Runs the test specified by `icase` simultaneously `parallel` (default=6) times
until an error occurs. Useful for hunting flakes.

Example:
```shell
$  make flakehunter-parallel icase='(data_loss_protection|channel_backup)' backend=neutrino
```

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

`sample-conf-check`
-------------------
Checks whether all required options of `lnd --help` are included in [sample-lnd.conf](github.com/lightningnetwork/lnd/blob/master/sample-lnd.conf) and that the default values of `lnd --help` are also mentioned correctly.

`scratch`
---------
Compiles all dependencies and builds the `./lnd` and `./lncli` binaries.
Equivalent to [`lint`](#lint) [`btcd`](#btcd)
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
Runs the unit test suite with test coverage, compiling the statistics in
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
[gometalinter]: https://gopkg.in/alecthomas/gometalinter.v1 (gopkg.in/alecthomas/gometalinter.v1)
