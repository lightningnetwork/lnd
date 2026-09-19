# RPC Deprecation Policy

This document describes the default lifecycle for deprecating and removing
public RPC fields and methods. The goal is to give callers enough time to
migrate while avoiding silent behavior changes when old protobuf clients talk to
newer `lnd` versions.

The policy is strictest for request fields that affect behavior. Examples
include fields that constrain routing, fees, amounts, channel selection, safety
checks, authentication, authorization, or whether an operation is attempted at
all.

## Why Removal Needs a Parseable Rejection Phase

Removing a protobuf field and marking its tag as `reserved` prevents future
schema reuse, but it does not let server code detect that an old client set the
removed field. For binary gRPC, the value is decoded into the protobuf unknown
field set. Normal generated accessors and request validation will not see it.

For behavior-affecting request fields this can cause silent behavior changes:
an old client may believe it set a constraint, while the new server silently
ignores the unknown tag and processes the request without that constraint. To
avoid this, such fields should remain in the protobuf schema for one release
where use of the field is rejected explicitly before final removal.

## Request Field Lifecycle

Behavior-affecting request fields should follow this four-stage lifecycle across
major releases:

1. **Major N: deprecate**

   Keep the field in the protobuf schema and mark it `[deprecated = true]`.
   Update the field comment to name the replacement. Add release notes that
   explain the replacement and migration path.

   If the old and new fields have equivalent semantics, the server may continue
   accepting the deprecated field and translating it to the new behavior during
   this stage.

2. **Major N+1: announce rejection**

   Keep accepting the field, but add a release note warning that the field will
   stop being honored in the next major release. The warning should name the
   release where explicit use will begin returning an error.

3. **Major N+2: reject while still parseable**

   Keep the deprecated field in the protobuf schema, but stop honoring it. If
   the client sets the field to a non-default value, return a clear
   `InvalidArgument` error that identifies the deprecated field and names the
   replacement field.

   This stage intentionally keeps old generated clients wire-compatible enough
   for `lnd` to fail loudly instead of silently ignoring the value.

4. **Major N+3: remove and reserve**

   Remove the field from the protobuf message. Reserve both the numeric tag and
   the field name so neither can be reused accidentally.

   Remove the rejection logic and any compatibility translation code that was
   only needed while the field was still parseable.

This means that, by default, a behavior-affecting request field is removed from
the protobuf schema three major releases after it is first deprecated.

## Response Fields

Response-only fields may use a shorter lifecycle when dropping the field cannot
change server behavior and old clients can safely tolerate the field becoming
unset or absent. The release notes should still announce the deprecation and
removal schedule.

If a response field is used by clients for safety-critical decisions, wallet
accounting, channel state, payment state, or other operational behavior, prefer
the request-field lifecycle unless there is a clear reason to use a shorter
path.

## RPC Methods

Deprecated RPC methods should follow the same release cadence when practical:

1. Mark the method deprecated and document the replacement.
2. Announce the future rejection or removal in release notes.
3. Prefer returning a clear error for one major release if the method can remain
   registered without creating confusing behavior. The error should identify
   the deprecated RPC method and name the replacement RPC method.
4. Remove the method and corresponding REST annotations only after the announced
   removal release.

If keeping the method registered is impractical, the release notes must be
explicit about when the method will disappear and which replacement should be
used.
