# lnd - lightning network daemon

This repo is preliminary work on a lightning network peer to peer node and wallet.

It currently is being designed for testnet-L, a test network where all* txids are normalized.  This fixes malleability but isn't something that can be cleanly done to the existing bitcoin network.

It is not yet functional, but we hope to have a proof of concept on testnet-L soon.


### cmd / lncli
A command line to query and control a running lnd.  Similar to bitcoin-cli

### elkrem
Library to send and receive a tree structure of hashes which can be sequentially revealed.  If you want to send N secrets, you only need to send N secrets (no penalty there) but the receiver only needs to store log2(N) hashes, and can quickly compute any previous secret from those.

This is useful for the hashed secrets in LN payment channels.

### lndc
Library for authenticated encrypted communication between LN nodes.  It uses chacha20_poly1305 for the symmetric cipher, and the secp256k1 curve used in bitcoin for public keys.  No signing is used, only two ECDH calculations: first with ephemeral keypairs and second with persistent identifying public keys.

### uspv
Wallet library to connect to bitcoin nodes and build a local SPV and wallet transaction state.


