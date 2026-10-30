# Release Notes
- [Bug Fixes](#bug-fixes)
- [New Features](#new-features)
    - [Functional Enhancements](#functional-enhancements)
    - [RPC Additions](#rpc-additions)
    - [lncli Additions](#lncli-additions)
- [Improvements](#improvements)
    - [Functional Updates](#functional-updates)
    - [RPC Updates](#rpc-updates)
    - [lncli Updates](#lncli-updates)
    - [Breaking Changes](#breaking-changes)
    - [Performance Improvements](#performance-improvements)
    - [Deprecations](#deprecations)
- [Technical and Architectural Updates](#technical-and-architectural-updates)
    - [BOLT Spec Updates](#bolt-spec-updates)
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)

### ⚠️ Action required for Linux operators: CVE-2026-98432

[CVE-2026-98432](https://nvd.nist.gov/vuln/detail/CVE-2026-98432) is a Linux
kernel vulnerability. The [demonstrated remote
exploit](https://www.openwall.com/lists/oss-security/2026/10/30/1) requires a
Multipath TCP (MPTCP) listener, which Go 1.24+ enables by default and which lnd
0.20.x therefore exposed on its peer, gRPC and REST ports. Windows and macOS
are not affected.

Do all three, in order:

1. **Upgrade to this release.** lnd no longer opens MPTCP listeners, which
   closes the demonstrated exploit path through lnd.
2. **Disable MPTCP on the host.** This release does not protect other
   services on the same machine. Run:
   ```shell
   sudo sysctl -w net.mptcp.enabled=0
   echo 'net.mptcp.enabled = 0' | sudo tee /etc/sysctl.d/90-disable-mptcp.conf
   ```
   If the command reports that the key does not exist, your kernel has no
   MPTCP support and is not exposed through this path. The setting is per
   network namespace: if lnd runs in Docker, pass
   `--sysctl net.mptcp.enabled=0` to the container, or use the firewall rule
   in [issue 11455](https://github.com/lightningnetwork/lnd/issues/11455),
   which covers all namespaces.
3. **Install a patched kernel** as soon as your distribution ships one.
   Disabling MPTCP closes the demonstrated exploit only; the kernel bug is not
   specific to MPTCP, and the kernel patch is the only fix.

Verify with `sudo ss -Mlnp | grep lnd`, which should print nothing. If you
cannot upgrade yet, start the existing binary with `GODEBUG=multipathtcp=0`
instead; see the [pinned
mitigation](https://github.com/lightningnetwork/lnd/issues/11455#issuecomment-6523668359).
Do not re-enable MPTCP with `GODEBUG=multipathtcp=1` until your kernel is
patched.

# Bug Fixes

* lnd binaries now [disable MPTCP listeners by
  default](https://github.com/lightningnetwork/lnd/pull/11456), restoring plain
  TCP as the default transport and mitigating the demonstrated
  CVE-2026-98432 exploit path on vulnerable Linux kernels. Upgrading to a
  patched kernel is still required to fix the underlying vulnerability. Once
  the kernel is patched, operators can opt in to MPTCP again with
  `GODEBUG=multipathtcp=1`.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Code Health

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Ziggie
