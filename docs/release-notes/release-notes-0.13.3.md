# Release Notes

## Wallet

* [The `DefaultDustLimit` method has been removed in favor of `DustLimitForSize` which calculates the proper network dust limit for a given output size. This also fixes certain APIs like SendCoins to be able to send 294 sats to a P2WPKH script.](https://github.com/lightningnetwork/lnd/pull/5781)

# Contributors (Alphabetical Order)

* Eugene Siegel