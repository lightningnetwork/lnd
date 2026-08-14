# Security Policy

## Supported Versions

Lightning Labs maintains the <ins>**two most recent lnd release lines**</ins>: the current major line and the one immediately before it. Both maintained lines receive security fixes. When a fix lands on the current line and applies to the previous line, it is normally backported and shipped in a minor release on that line.

lnd releases are tagged `v0.MAJOR.MINOR-beta` — the middle component is the major version, so `v0.21` and `v0.20` are different release lines.

When a new major release ships, the older of the two maintained lines reaches end of life that day. An end-of-life line receives **no further releases of any kind, including security fixes**. If a vulnerability affects an end-of-life line, the remedy is an upgrade to a maintained line.

> [!IMPORTANT]
> We recommend running the latest minor release of the most recent major line you are able to upgrade to. A maintained line only protects you if you are on its latest minor release.

The full support policy, including the per-line maintenance table and the advisory disclosure schedule, is published at https://security.lightning.engineering/lifecycle/

## Reporting a Vulnerability

To report security issues, send an email to security@lightning.engineering (this list isn't to be used for support). 

The following key can be used to communicate sensitive information: [`91FE 464C D751 01DA 6B6B  AB60 555C 6465 E5BC B3AF`](https://gist.githubusercontent.com/Roasbeef/6fb5b52886183239e4aa558f83d085d3/raw/1ecb328bbcf36f76ead67f08008f8db1da07e60e/security@lightning.engineering). 
