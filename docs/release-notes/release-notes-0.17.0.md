# Release Notes

## DB

* Split channeldb [`UpdateInvoice`
  implementation](https://github.com/lightningnetwork/lnd/pull/7377) logic in 
  different update types.

## Watchtowers 

* Let the task pipeline [only carry 
  wtdb.BackupIDs](https://github.com/lightningnetwork/lnd/pull/7623) instead of 
  the entire retribution struct. This reduces the amount of data that needs to 
  be held in memory. 
 
## Misc

* [Ensure that both the byte and string form of a TXID is populated in the 
  lnrpc.Outpoint message](https://github.com/lightningnetwork/lnd/pull/7624). 

# Contributors (Alphabetical Order)

* Elle Mouton
* Jordi Montes

