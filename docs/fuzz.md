# Fuzzing LND #

The `fuzz` package is organized into subpackages which are named after the `lnd` package they test. Each subpackage has its own set of fuzz targets.

## Setup and Installation ##
This section will cover setup and installation of the fuzzing binaries.

* The following is a command to build all fuzzing harnesses:
```shell
⛰  make fuzz-build
```

* This may take a while since this will create zip files associated with each fuzzing target.

* The following is a command to run all fuzzing harnesses for 30 seconds:
```shell
⛰  make fuzz-run
```

`go-fuzz` will print out log lines every couple of seconds. Example output:
```text
2017/09/19 17:44:23 workers: 8, corpus: 23 (3s ago), crashers: 1, restarts: 1/748, execs: 400690 (16694/sec), cover: 394, uptime: 24s
```
Corpus is the number of items in the corpus. `go-fuzz` may add valid inputs to
the corpus in an attempt to gain more coverage. Crashers is the number of inputs
resulting in a crash. The inputs, and their outputs are logged by default in:
`fuzz/<package>/<harness>/crashers`. `go-fuzz` also creates a `suppressions` directory
of stacktraces to ignore so that it doesn't create duplicate stacktraces.
Cover is a number representing edge coverage of the program being fuzzed.

## Options ##
Several parameters can be appended to the end of the make commands to tune the build process or the way the fuzzer runs.
- `run_time` specifies how long each fuzz harness runs for. The default is 30 seconds.
- `timeout` specifies how long an individual testcase can run before raising an error. The default is 20 seconds.
- `processes` specifies the number of parallel processes to use while running the harnesses.
- `pkg` specifies the `lnd` packages to build or fuzz. The default is to build and run all available packages (`brontide lnwire wtwire zpay32`). This can be changed to build/run against individual packages.
- `base_workdir` specifies the workspace of the fuzzer. This folder will contain the corpus, crashers, and suppressions.

## Corpus ##
Fuzzing generally works best with a corpus that is of minimal size while achieving the maximum coverage. `go-fuzz` automatically minimizes the corpus in-memory before fuzzing so a large corpus shouldn't make a difference.

## Disclosure ##
If you find any crashers that affect LND security, please disclose with the information found [here](https://github.com/lightningnetwork/lnd/#security).
