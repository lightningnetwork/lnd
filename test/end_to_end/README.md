# Simple environment and integration tests for lnd

Contains simnet envirnment and end-to-end automated tests.
Note: lnd should be installed!

## Simnet environment

Directory contains in `simnet` configured simnet test network suitable for manual usage.
`simnet/btcd` - btcd simnet configuration. To launch btcd go into `simnet/btcd` and launch `start-btcd.sh` . Mining address in one of wallet's addresses. 
`simnet/btcwallet` - preconfigured btcwallet. To launch btcwallet go into `simnet/btcwallet` and launch `start-btcwallet.sh` Wallet passphrase is "lol". Wallet already have some coins.
`simnet/btcctl` - preconfigured btcctl client. Use it to control btcd and btcwallet. Go into `simnet/btcctl` and `btcctl.sh <your-commands>`. For example to generate 1 block do `./btcctl.sh generate 1`
`simnet/lnd-node1` - First lnd node. To start it go into `simnet/lnd-node1` and launch `start-lnd.sh`. RPC port: 10009, Peer port 10011
`simnet/lnd-node2` - Second lnd node. To start it go into `simnet/lnd-node2` and launch `start-lnd.sh`. RPC port: 11009, Peer port 11011
`simnet/lnd-node3` - Third lnd node. To start it go into `simnet/lnd-node3` and launch `start-lnd.sh`. RPC port: 12009, Peer port 12011
`simnet/lnd-node4` - Forth lnd node. To start it go into `simnet/lnd-node4` and launch `start-lnd.sh`. RPC port: 13009, Peer port 13011
`simnet/lnd-node5` - Fifth lnd node. To start it go into `simnet/lnd-node5` and launch `start-lnd.sh`. RPC port: 14009, Peer port 14011
REMEMBER: Each launch script should be called from specific directory.
By default `lncli` connects to first node. Use `--rpcserver` to specify needed node. For example to connect to 3 node use `--rpcserver localhost:12009`

Example usage scenario (to see how payment between two nodes work):
1. Launch 5 different terminals
2. Launch btcd, btcwallet, lnd-node1, lnd-node2. In each terminal go into specific directory and launch script.
3. Go into `simnet/btcctl`
4. Generate some address for first node
`lncli newaddress p2wkh`
5. Send some coins to this address from wallet
`./btcctl.sh walletpassphrase lol 1000`
`./btcctl.sh sendtoaddress <generated address> 10` - send 10 bitcoins to first node
`./btcctl.sh generate 10` - generate 10 blocks
6. Connect first node to second. 
Go to terminal with the output of second node. Find there a string like `identity address: <address>`. Example: `17:21:15 2016-08-23 [INF] LTND: identity address: SXWewg7yvFdp82tGFiNuQx2Ad91pxCrDKP`
`lncli connect <address>@localhost:11011`. - it will connect 1st node to second. Example: `lncli connect SXWewg7yvFdp82tGFiNuQx2Ad91pxCrDKP@localhost:11011`. Result contains peer_id of second node.
7. Create channel between nodes
`lncli openchannel --peer_id 1 --local_amt 100000000 --num_confs 1` - creates channel between first and second node. Funded solely by first node. Amount 1 BTC
`lncli generate 11` - generate blocks to include funding transaction into blockchain
8. Send payment:
`lncli --rpcserver localhost:11009 getinfo` - get lightning_id of the second node
`lncli sendpayment --dest <lightning_id> --amt 100` - send 100 Satoshi
9. To check lightning balance use `lncli listpeers`. It outputs peers with opened with them channels(which also include balance).

## Automated tests
They should be launched from this directory. For example `go run test.go`
They are useful during development.
They copy test environment in some temp directory and launch some command line operations like create channel, send money, test balance
`test.go` - test one-hop payments. 
`test_multihop.go` - test that routing(finding neighborhood nodes) works in small network
Note: launching this files will kill all running lnd, btcd, btcwallet !!!