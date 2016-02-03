# uspv - micro-SPV library

The uspv library implements simplified SPV wallet functionality.
It connects to full nodes using the standard port 8333 bitcoin protocol,
gets headers, uses bloom filters, gets blocks and transactions, and has
functions to send and receive coins.

## Files

Three files are used by the library:

#### Key file (currently testkey.hex)

This file contains the secret seed which creates all private keys used by the wallet.  It is stored in ascii hexadecimal for easy copying and pasting.  If you don't enter a password when prompted, you'll get a warning and the key file will be saved in the clear with no encryption.  You shouldn't do that though.  When using a password, the key file will be longer and use the scrypt KDF and nacl/secretbox to protect the secret seed.

#### Header file (currently headers.bin)

This is a file storing all the block headers.  Headers are 80 bytes long, so this file's size will always be an even multiple of 80.  All blockchain-technology verifications are performed when appending headers to the file.  In the case of re-orgs, since it's so quick to get headers, it just truncates a bit and tries again.

#### Database file (currently utxo.db)

This file more complex.  It uses bolt DB to store wallet information needed to send and receive bitcoins.  The database file is organized into 4 main "buckets":

* Utxos ("DuffelBag")

This bucket stores all the utxos.  The goal of bitcoin is to get lots of utxos, earning a high score.

* Stxos ("SpentTxs")

For record keeping, this bucket stores what used to be utxos, but are no longer "u"txos, and are spent outpoints.  It references the spending txid.

* Txns  ("Txns")

This bucket stores full serialized transactions which are refenced in the Stxos bucket.  These can be used to re-play transactions in the case of re-orgs.

* State ("MiscState")

This has describes some miscellaneous global state variables of the database, such as what height it has synchronized up to, and how many addresses have been created.  (Currently those are the only 2 things stored)

## Synchronization overview

Currently uspv only connects to one hard-coded node, as address messages and storage are not yet implemented.  It first asks for headers, providing the last known header (writing the genesis header if needed).  It loops through asking for headers until it receives an empty header message, which signals that headers are fully synchronized.

After header synchronization is complete, it requests merkle blocks starting at the keyfile birthday. (This is currently hard-coded; add new db key?)  Bloom filters are generated for the addresses and utxos known to the wallet.  If too many false positives are received, a new filter is generated and sent. (This happens fairly often because the filter exponentially saturates with false positives when using BloomUpdateAll.)   Once the merkle blocks have been received up to the header height, the wallet is considered synchronized and it will listen for new inv messages from the remote node.  An inv message describing a block will trigger a request for headers, starting the same synchronization process of headers then merkle-blocks.

## TODO

There's still quite a bit left, though most of it hopefully won't be too hard.  

Problems / still to do:

* Only connects to one node, and that node is hard-coded.
* Re-orgs affect only headers, and don't evict confirmed transactions.
* Double spends are not detected; Double spent txs will stay at height 0.
* Tx creation and signing is still very rudimentary.
* There may be wire-protocol irregularities which can get it kicked off.

Hopefully I can get most of that list deleted soon.

(Now sanity checks txs, but can't check sigs... because it's SPV.  Right.)

Later functionality to implement:

* "Desktop Mode" SPV, or "Unfiltered" SPV or some other name

This would be a mode where uspv doesn't use bloom filters and request merkle blocks, but instead grabs everything in the block and discards most of the data.  This prevents nodes from learning about your utxo set.  To further enhance this, it should connect to multiple nodes and relay txs and inv messages to blend in.

* Ironman SPV

Never request txs.  Only merkleBlocks (or in above mode, blocks).  No unconfirmed transactions are presented to the user, which makes a whole lot of sense as with unconfirmed SPV transactions you're relying completely on the honesty of the reporting node.