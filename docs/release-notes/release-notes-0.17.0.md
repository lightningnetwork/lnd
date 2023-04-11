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

* [Generate default macaroons
independently](https://github.com/lightningnetwork/lnd/pull/7592) on wallet
unlock or create.

# Contributors (Alphabetical Order)

* Daniel McNally
* Elle Mouton
* Jordi Montes
