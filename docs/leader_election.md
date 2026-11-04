# Increasing LND reliability by clustering

Normally LND nodes use the embedded bbolt database to store all important states.
This method of running has been proven to work well in a variety of environments,
from mobile clients to large nodes serving hundreds of channels. With scale however
it is desirable to be able to replicate LND's state to quickly and reliably move nodes,
do updates and be more resilient to datacenter failures.

Before v0.22, it was possible to store all essential state in a replicated etcd
DB and to run multiple LND nodes on different machines where only one of them
(the leader) is able to read and mutate the database. If the leader fails or is
decommissioned, a follower is elected and comes online to minimize downtime.

Leader election uses etcd independently of the database backend. PostgreSQL can
store the shared node state while etcd handles election; see the
[PostgreSQL setup guide](postgres.md).

## Building LND with leader election support

To create a dev build of LND with leader election support use the following command:

```shell
# Leaders share PostgreSQL state and use etcd to elect one writer.
$  make tags="kvdb_etcd kvdb_postgres"
```

## Running a local etcd instance for testing

To start your local etcd instance for testing run:

```shell
$  ./etcd \
    --auto-tls \
    --advertise-client-urls=https://127.0.0.1:2379 \
    --listen-client-urls=https://0.0.0.0:2379 \
    --max-txn-ops=16384 \
    --max-request-bytes=104857600
```

The large `max-txn-ops` and `max-request-bytes` values are currently recommended
but may not be required in the future.

## Configuring pre-v0.22 LND to use etcd storage and leader election

The example below applies to releases before v0.22. The etcd database backend
is disabled in v0.22; etcd remains supported for leader election with PostgreSQL
storage. Changing `db.backend` does not migrate existing etcd data. Migration
support is separate from this change.

To run LND with etcd, additional configuration is needed, specified either
through command line flags or in `lnd.conf`.

Sample command line:

```shell
$  ./lnd-debug \
    --db.backend=etcd \
    --db.etcd.host=127.0.0.1:2379 \
    --db.etcd.certfile=/home/user/etcd/bin/default.etcd/fixtures/client/cert.pem \
    --db.etcd.keyfile=/home/user/etcd/bin/default.etcd/fixtures/client/key.pem \
    --db.etcd.insecure_skip_verify \
    --cluster.enable-leader-election \
    --cluster.leader-elector=etcd \
    --cluster.etcd-election-prefix=cluster-leader \
    --cluster.id=lnd-1
```
The `cluster.etcd-election-prefix` option sets the election's etcd key prefix. 
The `cluster.id` is used to identify the individual nodes in the cluster
and should be set to a different value for each node.

Optionally users can specify `db.etcd.user` and `db.etcd.pass` for db user
authentication. If the database is shared, it is possible to separate our data
from other users by setting `db.etcd.namespace` to an (already existing) etcd
namespace. In order to test without TLS, we can set `db.etcd.disabletls`
flag to `true`.

Once the node is up and running we can start more nodes with the same command line.

## Identifying the leader node

The above setup is useful for testing but is not viable when running in a production
environment. For users relying on containers and orchestration services, it is
essential to know which node is the leader to be able to automatically route
network traffic to the right instance. For example in Kubernetes, the load balancer
will route traffic to all "ready" nodes. This readiness may be monitored by a
readiness probe.

For readiness probing we can simply use LND's state RPC service where a special state
`WAITING_TO_START` indicates that the node is waiting to become the leader and is
not started yet. To test this we can simply curl the REST endpoint of the state RPC:

```
readinessProbe:
    exec:
      command: [
        "/bin/sh",
        "-c",
        "set -e; set -o pipefail; curl -s -k -o - https://localhost:8080/v1/state | jq .'State' | grep -E 'NON_EXISTING|LOCKED|UNLOCKED|RPC_ACTIVE|SERVER_ACTIVE'",
      ]
    periodSeconds: 1
```

## What data is written to the replicated remote database? 

When using PostgreSQL as a remote database, all
LND data will be written to the replicated database, including the wallet data
which contains the key material and node identity, the graph, the channel state,
the macaroon and the watchtower client databases. This means that when using
leader election there's no need to copy anything between instances of the LND
cluster.

## Is leader election supported for Postgres?

No, leader election is not supported by Postgres itself since it doesn't have a
mechanism to reliably **determine a leading node**. It is, however, possible to
use Postgres **as the LND database backend** while using an etcd cluster purely
for the leader election functionality. 
