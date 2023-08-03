# Release Notes

## Code Health

* [Remove Litecoin code](https://github.com/lightningnetwork/lnd/pull/7867). 
  With this change, the `Bitcoin.Active` config option is now deprecated since
  Bitcoin is now the only supported chain. The `chains` field in the 
  `lnrpc.GetInfoResponse` message along with the `chain` field in the 
  `lnrpc.Chain` message have also been deprecated for the same reason. 

# Contributors (Alphabetical Order)

* Elle Mouton
