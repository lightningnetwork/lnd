# Fuzzing LND #

The `fuzz` package is organized into subpackages which are named after the `lnd` package they test. Each subpackage has its own set of fuzz targets.

### Setup and Installation ###
This section will cover setup and installation of `go-fuzz` and fuzzing binaries.

* First, we must get `go-fuzz`.
```
$ go get -u github.com/dvyukov/go-fuzz/...
```
* The following is a command to build all fuzzing harnesses for a specific package.
```
$ cd fuzz/<package>
$ find * -maxdepth 1 -regex '[A-Za-z0-9\-_.]'* -not -name fuzz_utils.go | sed 's/\.go$//1' | xargs -I % sh -c 'go-fuzz-build -func Fuzz_% -o <package>-%-fuzz.zip github.com/lightningnetwork/lnd/fuzz/<package>'
```

* This may take a while since this will create zip files associated with each fuzzing target.

* Now, run `go-fuzz` with `workdir` set as below!
```
$ go-fuzz -bin=<.zip archive here> -workdir=<harness> -procs=<num workers>
```

`go-fuzz` will print out log lines every couple of seconds. Example output:
```
2017/09/19 17:44:23 workers: 8, corpus: 23 (3s ago), crashers: 1, restarts: 1/748, execs: 400690 (16694/sec), cover: 394, uptime: 24s
```
Corpus is the number of items in the corpus. `go-fuzz` may add valid inputs to
the corpus in an attempt to gain more coverage. Crashers is the number of inputs
resulting in a crash. The inputs, and their outputs are logged in:
`fuzz/<package>/<harness>/crashers`. `go-fuzz` also creates a `suppressions` directory
of stacktraces to ignore so that it doesn't create duplicate stacktraces.
Cover is a number representing edge coverage of the program being fuzzed.

### Brontide ###
The brontide fuzzers need to be run with a `-timeout` flag of 20 seconds or greater since there is a lot of machine state that must be printed on panic. 

### Corpus ###
Fuzzing generally works best with a corpus that is of minimal size while achieving the maximum coverage. However, `go-fuzz` automatically minimizes the corpus in-memory before fuzzing so a large corpus shouldn't make a difference - edge coverage is all that really matters.

### Test Harness ###
If you take a look at the test harnesses that are used, you will see that they all consist of one function: 
```
func Fuzz(data []byte) int
```
If:

- `-1` is returned, the fuzzing input is ignored
- `0` is returned, `go-fuzz` will add the input to the corpus and deprioritize it in future mutations.
- `1` is returned, `go-fuzz` will add the input to the corpus and prioritize it in future mutations.

### Conclusion ###
Citizens, do your part and `go-fuzz` `lnd` today!
