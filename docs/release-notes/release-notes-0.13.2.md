# Release Notes

## AMP

A new command line option (`--amp-reuse`) has been added to make it easier for
users to re-use AMP invoice on the command line using either the `payinvoice`
or `sendpayment` command.

## Bug Fixes

[A bug has been fixed in the command line argument parsing for the
`sendpayment` command that previously prevented users from being able to re-use
a fully
specified AMP](https://github.com/lightningnetwork/lnd/pull/5554) invoice by
generating a new `pay_addr` and including it as a CLI arg.

# Contributors (Alphabetical Order)
* Olaoluwa Osuntokun
