# SQLite support in LND

With the introduction of the `kvdb` interface, LND can support multiple database
backends. One of the supported backends is [sqlite](https://www.sqlite.org/index.html). This document
describes how use LND with a sqlite backend.

## Configuring LND for SQLite

LND is configured for SQLite through the following configuration options:

* `db.backend=sqlite` to select the SQLite backend.
* `db.sqlite.filename=...` to set the file where sqlite data will be stored. 
  This file will be created if it does not exist.
* `db.sqlite.timeout=...` to set the connection timeout. If not set, no
  timeout applies.

Example as follows:
```
[db]
db.backend=sqlite
db.sqlite.dsn=/var/data/lnd.db
db.sqlite.timeout=0
```