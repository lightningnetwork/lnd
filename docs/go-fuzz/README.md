# How to fuzz the Lightning Network Daemon's wire protocol using go-fuzz #

This document will describe how to use the fuzz-testing library `go-fuzz` on
the `lnd` wire protocol.

### Introduction ###

Lnd uses its own wire protocol to send and receive messages of all types. There
are 22 different message types, each with their own specific format. If a
message is not in the correct format, `lnd` should logically reject the message
and throw an error. But what if it doesn't? What if we could sneakily craft a
custom message that could pass all the necessary checks and cause an error to
go undetected? Chaos would ensue. However, crafting such a message would require
an in-depth understanding of the many different cogs that make the wire protocol
tick.

A better solution is fuzz-testing. Fuzz-testing or fuzzing is when a program
known as a fuzzer generates many, many inputs to a function or program in an
attempt to cause it to crash. Fuzzing is surprisingly effective at finding bugs
and a particular fuzzing program `AFL` is well-known for the amount of bugs it
has found with its learned approach. The library we will be using, `go-fuzz`, is
based on `AFL` and has quite a track record of finding bugs in a diverse set of
go programs. `go-fuzz` takes a coverage-guided approach in an attempt to cover
as many code paths as possible on an attack surface. We give `go-fuzz` real,
valid inputs and it will essentially change bits until it achieves a crash!
After reading this document, you too may be able to find errors in `lnd` with
`go-fuzz`!

### Setup and Installation ###
This section will cover setup and installation of `go-fuzz`.

* First, we must get `go-fuzz`:
```
$ go get github.com/dvyukov/go-fuzz/go-fuzz
$ go get github.com/dvyukov/go-fuzz/go-fuzz-build
```
* Next, create a folder in the `lnwire` package. You can name it whatever.
```
$ mkdir lnwire/<folder name here>
```
* Unzip `corpus.tar.gz` in the `docs/go-fuzz` folder and move it to the folder you just made.
```
$ tar -xzf docs/go-fuzz/corpus.tar.gz
$ mv corpus lnwire/<folder name here>
```
* Now, move `wirefuzz.go` to the same folder you just created.
```
$ mv docs/go-fuzz/wirefuzz.go lnwire/<folder name here>
```
* Change the package name in `wirefuzz.go` from `wirefuzz` to `<folder name here>`.
* Build the test program - this produces a `<folder name here>-fuzz.zip` (archive) file.
```
$ go-fuzz-build github.com/lightningnetwork/lnd/lnwire/<folder name here>
```
* Now, run `go-fuzz`!!!
```
$ go-fuzz -bin=<.zip archive here> -workdir=lnwire/<folder name here>
```

`go-fuzz` will print out log lines every couple of seconds. Example output:
```
2017/09/19 17:44:23 slaves: 8, corpus: 23 (3s ago), crashers: 1, restarts: 1/748, execs: 400690 (16694/sec), cover: 394, uptime: 24s
```
Corpus is the number of items in the corpus. `go-fuzz` may add valid inputs to
the corpus in an attempt to gain more coverage. Crashers is the number of inputs
resulting in a crash. The inputs, and their outputs are logged in:
`<folder name here>/crashers`. `go-fuzz` also creates a `suppressions` directory
of stacktraces to ignore so that it doesn't create duplicate stacktraces.
Cover is a number representing coverage of the program being fuzzed. When I ran
this earlier, `go-fuzz` found two bugs ([#310](https://github.com/lightningnetwork/lnd/pull/310) and [#312](https://github.com/lightningnetwork/lnd/pull/312)) within minutes!

### Corpus Notes ###
You may wonder how I made the corpus that you unzipped in the previous step.
It's quite simple really. For every message type that `lnwire_test.go`
processed in `TestLightningWireProtocol`, I logged it (in `[]byte` format) to
a .txt file. Within minutes, I had a corpus of valid `lnwire` messages that
I could use with `go-fuzz`! `go-fuzz` will alter these valid messages to create
the sneakily crafted message that I described in the introduction that manages
to bypass validation checks and crash the program. I ran `go-fuzz` for several
hours on the corpus I generated and found two bugs. I believe I have exhausted
the current corpus, but there are still perhaps possible malicious inputs that
`go-fuzz` has not yet reached and could reach with a slightly different generated
corpus.

### Test Harness ###
If you take a look at the test harness that I used, `wirefuzz.go`, you will see
that it consists of one function: `func Fuzz(data []byte) int`. `go-fuzz` requires
that each input in the corpus is in `[]byte` format. The test harness is also
quite simple. It reads in `[]byte` messages into `lnwire.Message` objects,
serializes them into a buffer, deserializes them back into `lnwire.Message` objects
and asserts their equality. If the pre-serialization and post-deserialization
`lnwire.Message` objects are not equal, the wire protocol has encountered a bug.
Wherever a `0` is returned, `go-fuzz` will ignore that input as it has reached
an unimportant code path caused by the parser catching the error. If a `1` is
returned, the `[]byte` input was parsed successfully and the two `lnwire.Message`
objects were indeed equal. This `[]byte` input is then added to the corpus as
a valid message. If a `panic` is reached, serialization or deserialization failed
and `go-fuzz` may have found a bug.

### Conclusion ###
Fuzzing is a powerful and quick way to find bugs in programs that works especially
well with protocols where there is a strict format with validation rules. Fuzzing
is important as an automated security tool and can find real bugs in real-world
software. The fuzzing of `lnd` is by no means complete and there exist probably
many more bugs in the software that may `go` undetected if left unfuzzed.  Citizens,
do your part and `go-fuzz` `lnd` today!
