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

### ⚠️ **Warning:** Disabling MPTCP in lnd is only a mitigation, disable it at the kernel level

This release [disables MPTCP listeners in
lnd](https://github.com/lightningnetwork/lnd/pull/11456) to close the
demonstrated CVE-2026-98432 exploit path. This only changes how lnd itself
opens its sockets: it does not fix the vulnerable kernel code, and it does not
protect any other service running on the same host. The real fix is to upgrade
to a patched kernel. Until then, operators on Linux should switch off MPTCP
support system-wide with `sysctl -w net.mptcp.enabled=0`, and persist the
setting in `/etc/sysctl.d/` so that it survives a reboot. Do not opt back in
with `GODEBUG=multipathtcp=1` on an unpatched kernel.

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
