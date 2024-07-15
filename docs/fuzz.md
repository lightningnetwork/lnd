# Fuzzing LND #

The following runs all fuzz tests on default settings:
```shell
$  make fuzz
```
The following runs all fuzz tests inside the lnwire package, each for a total of 1 minute, using 4 procs. 
It is recommended that processes be set to the number of processor cores in the system:
```shell
$  make fuzz pkg=lnwire fuzztime=1m parallel=4
```
Alternatively, individual fuzz tests can be run manually by setting the working directory to the location of the .go file holding the fuzz tests.
The go test command can only test one fuzz test at a time:
```shell
$  cd lnwire
$  go test -fuzz=FuzzAcceptChannel -fuzztime=1m -parallel=4
```
The following can be used to show all fuzz tests in the working directory:
```shell
$  cd lnwire
$  go test -list=Fuzz.*
```

Fuzz tests can be run as normal tests, which only runs the seed corpus:
```shell
$  cd lnwire
$  go test -run=FuzzAcceptChannel -parallel=4
```
The generated corpus values can be found in the $(go env GOCACHE)/fuzz directory.
## Options ##
Several parameters can be appended to the end of the make commands to tune the build process or the way the fuzzer runs.
- `fuzztime` specifies how long each fuzz test runs for, corresponding to the `go test -fuzztime` option. The default is 30s.
- `parallel` specifies the number of parallel processes to use while running the harnesses, corresponding to the `go test -parallel` option.
- `pkg` specifies the `lnd` packages to build or fuzz. The default is to build and run all available packages (`brontide lnwire watchtower/wtwire zpay32`). This can be changed to build/run against individual packages.

## Corpus ##
Fuzzing generally works best with a corpus that is of minimal size while achieving the maximum coverage.

## Disclosure ##
If you find any crashers that affect LND security, please disclose with the information found [here](https://github.com/lightningnetwork/lnd/#security).
