# SQLite support in LND

With the introduction of the `kvdb` interface, LND can support multiple database
backends. One of the supported backends is 
[sqlite](https://www.sqlite.org/index.html). This document describes how use 
LND with a sqlite backend.

Note that for the time being, the sqlite backend option can only be set for 
_new_ nodes. Setting the option for an existing node will not migrate the data. 

## Supported platforms and architectures

Note that the sqlite backend is _not_ supported for Windows (386/ARM) or for 
Linux (PPC/MIPS) due to these platforms [not being supported by the sqlite 
driver library.](
https://pkg.go.dev/modernc.org/sqlite#hdr-Supported_platforms_and_architectures)

## Configuring LND for SQLite

LND is configured for SQLite through the following configuration options:

* `db.backend=sqlite` to select the SQLite backend.
* `db.sqlite.timeout=...` to set the connection timeout. If not set, no
  timeout applies.
* `db.sqlite.busytimeout=...` to set the maximum amount of time that a db call 
  should wait if the db is currently locked.
* `db.sqlite.pragmaoptions=...` to set a list of pragma options to be applied 
  to the db connection. See the 
  [sqlite documentation](https://www.sqlite.org/pragma.html) for more 
  information on the available pragma options.

## Default pragma options

Currently, the following pragma options are always set:

```
foreign_keys=on
journal_mode=wal
busy_timeout=5000 // Overried with the db.sqlite.busytimeout option.
```

The following pragma options are set by default but can be overridden using the
`db.sqlite.pragmaoptions` option:

``` 
synchronous=full
auto_vacuum=incremental
fullfsync=true // Only meaningful on a Mac. 
```

## Auto-compaction

To activate auto-compaction on startup, the `incremental_vacuum` pragma option
should be used:
```
// Use N to restrict the maximum number of pages to be removed from the 
// freelist.
db.sqlite.pragmaoptions=incremental_vacuum(N) 

// Omit N if the entire freelist should be cleared.
db.sqlite.pragmaoptions=incremental_vacuum
```

Example as follows:
```
[db]
db.backend=sqlite
db.sqlite.timeout=0
db.sqlite.busytimeout=10s
db.sqlite.pragmaoptions=temp_store=memory
db.sqlite.pragmaoptions=incremental_vacuum
```
