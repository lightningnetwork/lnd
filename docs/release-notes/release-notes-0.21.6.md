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

### ⚠️ **Action required for Linux operators:** disable MPTCP in the kernel

**If you run lnd on Linux, we strongly urge you to switch off MPTCP at the
kernel level in addition to upgrading to this release.**

This release [stops lnd from accepting MPTCP
connections](https://github.com/lightningnetwork/lnd/pull/11456), which closes
the demonstrated CVE-2026-98432 exploit path through lnd. This is a mitigation,
not a fix: the vulnerability lives in the Linux kernel itself, so every other
MPTCP-capable service on the same machine remains exposed. Unless you
knowingly depend on MPTCP, there is no reason to keep it enabled.

Disable MPTCP right away:

```shell
sudo sysctl -w net.mptcp.enabled=0
```

Keep it disabled across reboots:

```shell
echo 'net.mptcp.enabled = 0' | sudo tee /etc/sysctl.d/90-disable-mptcp.conf
```

Install a patched kernel as soon as your distribution ships one. Until then, do
not re-enable MPTCP for lnd with `GODEBUG=multipathtcp=1`.

# Bug Fixes

* lnd binaries now [disable MPTCP listeners by
  default](https://github.com/lightningnetwork/lnd/pull/11456), restoring plain
  TCP as the default transport and mitigating the demonstrated
  CVE-2026-98432 exploit path on vulnerable Linux kernels. Operators can opt
  in to MPTCP with `GODEBUG=multipathtcp=1`; upgrading to a patched kernel is
  still required to fix the underlying vulnerability.

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
