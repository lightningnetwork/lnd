# Release Notes

## Wallet

* [The `DefaultDustLimit` method has been removed in favor of `DustLimitForSize` which calculates the proper network dust limit for a given output size. This also fixes certain APIs like SendCoins to be able to send 294 sats to a P2WPKH script.](https://github.com/lightningnetwork/lnd/pull/5781)

## Safety

* [The `htlcswitch.Switch` has been modified to take into account the total dust sum on the incoming and outgoing channels before forwarding. After the defined threshold is reached, dust HTLC's will start to be failed. The default threshold is 500K satoshis and can be modified by setting `--dust-threshold=` when running `lnd`.](https://github.com/lightningnetwork/lnd/pull/5770)

# Contributors (Alphabetical Order)

* Eugene Siegel