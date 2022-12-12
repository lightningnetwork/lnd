# Integration Test

`itest` is a package that houses the integration tests made for `lnd`. This
package builds test cases using the test framework `lntest`.

## Add New Tests

To add a new test case, create a `TestFunc` and add it in `list_on_test.go`.
Ideally, the `Name` should just be the snake case of the name used in
`TestFunc` without the leading `test` and underscores. For instance, to test
`lnd`'s exporting channel backup, we have,

```go
{
		Name:     "export channel backup",
		TestFunc: testExportChannelBackup,
}
```

The place to put the code of the `TestFunc` is case-specific. `itest` package
has loosely defined a list of files to test different functionalities of `lnd`.
The new test needs to be put into one of these files, otherwise, a new file
needs to be created.

## Run Tests

#### Run a single test case

To run a single test case, use `make itest icase=$case`, where `case` is the
name defined in `list_on_test.go`, with spaces replaced with underscores(`_`).

```shell
# Run `testListChannels`.
make itest icase=list_channels
```

#### Run multiple test cases

There are two ways to run multiple test cases. One way is to use `make itest
icase=$cases`, where `cases` has the format `cases='(case|case|...)'`. The
`case` is the name defined in `list_on_test.go`, with spaces replaced with
underscores(`_`).

```shell
# Run `testListChannels` and `testListAddresses` together.
make itest icase='(list_channels|list_addresses)'
```

Another way to run multiple cases is similar to how Go runs its tests - by
simple regex matching. For instance, the following command will run three cases
since they all start with the word `list`,

```shell
# Run `testListChannels`, `testListAddresses`, and `testListPayments` together.
make itest icase=list
```

#### Run all tests

To run all tests, use `make itest` without `icase` flag.

```shell
# Run all test cases.
make itest
```

#### Run tests in parallel

To run tests in parallel, use `make itest-parallel`. This command takes two
special arguments,
- `tranches`, specifies the number of parts the test cases will be split into.
- `parallel`, specifies the number of threads to run in parallel. This value
  must be smaller than or equal to `tranches`.

```shell
# Split the tests into 4 parts, and run them using 2 threads.
make itest-parallel tranches=4 parallel=2
```

By default, `itest-parallel` splits the tests into 4 parts and uses 4 threads
to run each of them.

#### Additional arguments

For both `make itest` and `make itest-parallel`, the following arguments are
allowed,
- `timeout`, specifies the timeout value used in testing.
- `dbbackend`, specifies the database backend. Must be `bbolt`, `etcd`, or
  `postgres`, default to `bbolt`.
- `backend`, specifies the chain backend to be used. Must be one of,
	- `btcd`, the default value.
	- `neutrino`
	- `bitcoind`
	- `bitcoind notxindex`
	- `bitcoind rpcpolling`

```shell
# Run a single test case using bitcoind as the chain backend and etcd as the
# database backend, with a timeout of 5 minutes.
make itest icase=list_channels backend=bitcoind dbbackend=etcd timeout=5m

# Run all test cases in parallel, using bitcoind notxindex as the chain backend
# and etcd as the database backend, with a timeout of 60 minutes for each
# parallel.
make itest-parallel backend="bitcoind notxindex" dbbackend=etcd timeout=60m
```
