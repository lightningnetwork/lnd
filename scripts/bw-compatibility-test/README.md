# Basic Backwards Compatibility Test

This directory houses some docker compose files and various bash helpers all 
used by the main `test.sh` file which runs the test. The idea is to be able to 
test that a node can be upgraded from a stable release to a checked out branch
of LND and still function as expected with nodes running the stable release.

## Test Flow

The test sets up the following network:

```
Alice <---> Bob <---> Charlie <---> Dave
```

Initially, all the nodes are running a tagged LND release. This is all 
configured via the main `docker-compose.yaml` file. 

1. The Bob node is the node we will focus on. While Bob is still on a stable 
   version of LND, we ensure that he can: send and receive multi-hop payments as
   well as route payments.

2. Bob is then shutdown. 

3. The `docker-compose.override.yaml` file is then loaded and used to spin up
   Bob again but this time using the checked out branch of LND. This is done by 
   using the `dev.Dockerfile` in the LND repo. 

4. The test now waits for this new version of Bob (which uses the same data 
   directory as the previous version) to sync up with the network, reactivate 
   its channels.
 
5. Finally, basic send, receive and routing tests are run to ensure that Bob
   is still functional after the upgrade. 

## How to use this directory

1. If you would just like to run the full test from start to finish, then all 
   you need to do is run: 
  
   ```bash
    ./test.sh
   ```
   
2. If you would like to run the test in parts, then you can use the `execute.sh`
   script to call the various functions in the directory. Here is an example:
   ```bash
   # Spin up the docker containers.
   ./execute.sh compose_up
   
   # Wait for the nodes to start.
   ./execute.sh wait_for_nodes alice bob charlie dave
   
   # Query various nodes.
   ./execute.sh alice getinfo
   
   # Set-up a basic channel network.
   ./execute.sh setup-network
   
   # Wait for bob to see all the channels in the network.
    ./execute.sh wait_graph_sync bob 3
   
   # Open a channel between Bob and Charlie.
   ./execute.sh open_channel bob charlie
   
   # Send a payment from Alice to Dave.
   ./execute.sh send_payment alice dave
   
   # Take down a single node. 
   ./execute.sh compose_stop dave
   
   # Start a single node.
   ./execute.sh compose_start dave
   
   ```

## File Descriptions:

##### `test.sh`

This script runs the full backwards compatibility test. 

 ```bash
   ./test.sh
 ```

##### `execute.sh`

A helper script that allows you to call the various functions in the directory.

This is useful if you are debugging something and want to step through the test
steps manually while keeping the main network running.

For example: 
```bash
 # Spin up the docker containers.
 ./execute.sh compose_up
 
 # Query various nodes.
 ./execute.sh alice getinfo
```

#### `compose.sh`

This script contains all the docker-compose variables and helper functions that 
are used in the test.

#### `network.sh`

This script contains all the various helper functions that can be used to 
interact with the Lightning nodes in the network.

### `vars.sh`

Any global variables used across the other scripts are defined here.

#### `docker-compose.yaml`

This docker compose file contains the network configuration for all the nodes
running a stable LND tag. 

#### `docker-compose.override.yaml`

This compose file adds defines the `bob-pr` service which will pull and build
the LND branch that is currently checked out.
