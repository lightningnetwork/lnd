# Table of Contents
1. [Overview](#overview)
1. [Debug Logging](#debug-logging)
1. [LND's built-in profiler](#built-in-profiler-in-lnd)

## Overview

`lnd` ships with a few useful features for debugging, such as a built-in
profiler and tunable logging levels. If you need to submit a bug report
for `lnd`, it may be helpful to capture debug logging and performance
data ahead of time.

## Debug Logging

LND supports different logging levels and you can also specify different logging
levels per subsystem. This makes it easy to focus on a particular subsystem
without clogging up the logs with a lot of noise. One can either set the logging
in the lnd.conf file or pass the flag `--debuglevel` with the specified level 
when starting lnd.

LND supports the following logging levels (see [log.go](/build/log.go) and
[sample-lnd.conf](/sample-lnd.conf) for more info):

- `trace`
- `debug`
- `info`
- `warn`
- `error`
- `critical`
- `off`

LND is composed of many subsystems, those subsystems can be listed either by
setting the starting flag `--debuglevel` or by using the lncli program.

Show all subsystems:

```shell
$  lnd --debuglevel=show
$  lncli debuglevel --show
```
For more details see [log.go](/log.go).

You may also specify logging per-subsystem, like this:

```shell
$  lnd --debuglevel=<subsystem>=<level>,<subsystem2>=<level>,...
$  lncli debuglevel --level=<subsystem>=<level>,<subsystem2>=<level>,...
```
The default global logging level is `info`. So if one wants to change the
global logging level and in addition also set a more detailed logging for a 
particular subsystem the command would look like this (using `HSWC` 
(htlcswitch) as an example subsystem):

```shell
$ lnd --debuglevel=critical,HSWC=debug
$ lncli debuglevel --level=critical,HSWC=debug
```
The subsystem names are case-sensitive and must be all uppercase.

To identify the subsystems defined by an abbreviated name, you can search for
the abbreviation in the [log.go](/log.go) file. Each subsystem
declares a `btclog.Logger` instance locally which is then assigned via the
`UseLogger` function call in the `SetupLoggers` function.

Example HSWC:

For the `HSWC` subsystem a new sublogger is injected into the htlcswitch
package via the `UseLogger` function call in the `SetupLoggers` function. So
the HSWC subsystem handles the logging in the htlcswitch package.

```go
 AddSubLogger(root, "HSWC", interceptor, htlcswitch.UseLogger)
```

Caution: Some logger subsystems are overwritten during the instanziation. An
example here is the `neutrino/query` package which instead of using the `BTCN`
prefix is overwritten by the `LNWL` subsystem.

Moreover when using the `lncli` command the return value will provide the 
updated list of all subsystems and their associated logging levels. This makes
it easy to get an overview of the current logging level for the whole system.

Example:

```shell
$ lncli debuglevel --level=critical,HSWC=debug
{
    "sub_systems": "ARPC=INF, ATPL=INF, BLPT=INF, BRAR=INF, BTCN=INF, BTWL=INF, CHAC=INF, CHBU=INF, CHCL=INF, CHDB=INF, CHFD=INF, CHFT=INF, CHNF=INF, CHRE=INF, CLUS=INF, CMGR=INF, CNCT=INF, CNFG=INF, CRTR=INF, DISC=INF, DRPC=INF, FNDG=INF, GRPH=INF, HLCK=INF, HSWC=DBG, INVC=INF, IRPC=INF, LNWL=INF, LTND=INF, NANN=INF, NRPC=INF, NTFN=INF, NTFR=INF, PEER=INF, PRNF=INF, PROM=INF, PRPC=INF, RPCP=INF, RPCS=INF, RPWL=INF, RRPC=INF, SGNR=INF, SPHX=INF, SRVR=INF, SWPR=INF, TORC=INF, UTXN=INF, VRPC=INF, WLKT=INF, WTCL=INF, WTWR=INF"
}
```


## Built-in profiler in LND

`LND` has a built-in feature which allows you to capture profiling data at
runtime using [pprof](https://golang.org/pkg/runtime/pprof/), a profiler for
Go. It is recommended to enable the profiling server so that an analyis can be
triggered during runtime. There is only little overhead in enabling this 
feature, because profiling is only started when calling the server endpoints.
However LND also allows to specify a cpu profile file via the `cpuprofile` flag
which triggers a cpu profile when LND starts and stops it when LND shuts down.
This is only recommended for debugging purposes, because the overhead is much
higher. To enable the profile server, start `lnd` with the `--profile` option
using a free port. As soon as the server is up different profiles can be
fetched from the `debug/pprof` endpoint using either the web interface or for
example `curl`.

Example port `9736` is used for the profile server in the following examples.

```shell
$  lnd --profile=9736
```

NOTE: The `--profile` flag of the lncli program does not relate to profiling and
the profiling server. It has a different context and allows a node operator to
manage different LND daemons without providing all the cmd flags every time.
For more details see [lncli profile](/cmd/commands/profile.go).

### Different types of profiles

#### CPU profile

A cpu profile can be used to analyze the CPU usage of the program. When
obtaining it via the profile http endpoint you can specify the time duration as
a query parameter.

```shell
$ curl http://localhost:9736/debug/pprof/profile?seconds=10 > cpu.prof
```
#### Goroutine profile

The goroutine profile is very useful when analyzing deadlocks and lock 
contention. It can be obtained via the web interface or the following endpoint:

```shell
$ curl http://localhost:9736/debug/pprof/goroutine?debug=2 > goroutine.prof
```
The query parameter `debug=2` is optional but recommended and referes to the
format of the output file. Only this format has the necessary information to
identify goroutines deadlocks. Otherwise `go tool pprof` needs to be used to
visualize the data and interpret the results.

#### Heap profile

The heap profile is useful to analyze memory allocations. It can be obtained
via the following endpoint:

```shell
$ curl http://localhost:9736/debug/pprof/heap > heap.prof
```
The documentation of the pprof package states that a gc can be triggered before
obtaining the heap profile. This can be done by setting the gc query parameter
(`gc=1`).

#### Other profiles

There are several other options available like a mutex profile or a block
profile which gives insights into contention and bottlenecks of your program.
The web interface lists all the available profiles/endpoints which can be
obtained.

However mutex and block profiling need to be enabled separately by setting the
sampling rate via the config values `BlockingProfile` and `MutexProfile`. They
are off by default (0). These values represent sampling rates meaning that a
value of `1` will record every event leading to a significant overhead whereas
a sample rate of `n` will only record 1 out of nth events decreasing the
aggressiveness of the profiler.

Fetching the block and mutex profile:

```shell

$ curl http://localhost:9736/debug/pprof/mutex?debug=2

$ curl http://localhost:9736/debug/pprof/block?debug=2
```

The full programm command can also be fetched which shows how LND was started
and which flags were provided to the program.

```shell
$ curl http://localhost:9736/debug/pprof/cmdline > cmdline.prof
```

There are also other endpoints available see the
[pprof documentation](https://golang.org/pkg/runtime/pprof/) for more details.


#### Visualizing the profile dumps

It can be hard to make sense of the profile dumps by just looking at them
therefore the Golang ecosystem provides tools to analyze those profile dumps
either via the terminal or by visualizing them. One of the tools is
`go tool pprof`.

Assuming the profile was fetched via `curl` as in the examples above a nice
svg visualization can be generated for the cpu profile like this:

```shell
$ go tool pprof -svg cpu.prof > cpu.svg
```
Details how to interpret these visualizations can be found in the
[pprof documentation](https://github.com/google/pprof/blob/main/doc/README.md#interpreting-the-callgraph).