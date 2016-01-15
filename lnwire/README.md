Funding (segwit+CSV)
====================

This is two-party funder for a single Funding Transaction (more efficient and
makes the channel creation atomic, but doesn't work for
CSV-no-malleability-fix).

Funding Request
---------------
Someone wants to open a channel. The requester provides any inputs and relevant
information on how much they want to fund and the parameters, these paramters
are a proposal.

Funding Response
----------------
If the responder accepts the request, they also provide any inputs, and returns
with parameters as well. These parameters are now considered "Committed" and the
negotation has finished. If the requester doesn't agree with the new conditions,
they stop. The response also contains the first Commitment pubkey provided by the
responder, which refunds the initial balance back to both parties.

Funding SignAccept
------------
The requester now has sufficient information to get a refund if the transaction
is ever broadcast. The requester signs the Funding Transaction and this message
gives the signature to the responder. The requester also provides the signature
for the initial Commitment Transaction.

Funding SignComplete
---------------
The responder has sufficient information to broadcast the Funding Transaction
(with the ability to receive a refund), the responder broadcasts on the
blockchain and returns the txid to the requester, with the signature of the
Funding Transaction. This is provided as a courtesy, it cannot be relied upon
with non-cooperative channel counterparties and the Funding Transaction can be
braodcast without this message being received by the requester. After the
necessary number of confirmations, Lightning Network transactions can proceed.


Cooperative Channel Close
=========================

This is when either party want to close out a channel with the current balance.
Requires the cooperation of both parites for this type. In the event of
non-cooperation, either party may broadcast the most recent Commitment
Transaction.

Close Request 
-------------
One party unilaterally sends their sig and fee amount to the other party. No
further channel updates are possible. In the future, we might include HTLCs in
the outputs, but for now, we're assuming *all* HTLCs are cleared out.

Close Complete
----------------------
Returns the Txid and sig as a courtesy. The counterparty might not send this if
they're being non-cooperative.


Commitments and HTLCs 
=====================

This is designed to be non-blocking where there can be multiple Commitments per
person and the Commitments do not need to match. A HTLC is only believed to be
added when it's in both parties' most recent Commitment (same with
timeout/settle) and all prior Commitments not reflecting the change are revoked
by the counterparty.

As a result, there can easily be hundreds of state updates/payments per second
per channel.

Commitment States
-----------------

Commitments:
1. HTLCs, can be modified. Any add/settlement/timeout/etc. gets added to
   staging.
2. Signed, only one signed state at a time can exist per party. Takes HTLCs
   staging and locks it in, can now be broadcast on-chain by the counterparty.
3. Completed and Revoked, other party sends their revocation accepting this
   Commitment. Sending a revocation means you *ACCEPT* the Commitment. There
   should never be a case where a Commitment Signature happens and the client
   refusees to revoke -- instead the client should immediately close out the
   channel.
4. Deprecated, a commitment is old, marked as deprecated when there is a new
   Commitment and this one is revoked. These commitments never be broadcasted.
5. Invalid, close out channel immediately.

There can be multiple commitments going at a time per party (currently limits a
total of 16 possible in-flight that can be broadcast for sanity, but there's no
real limit).

For validity, all you do is ensure that the changes from the old commitment are
legit (based on your HTLC/staging data)
COMMIT\_STAGING
COMMIT\_SIGNED
COMMIT\_COMPLETE

Messages:
CommitSignature: Signature to establish COMMIT\_SIGNED state
CommitRevocation: Revoke prior states

ADD HTLCs
---------

Requester Add HTLC states (Adding HTLCs):
1. Pre-staged, don't know if the other person wants it
2. Staged, both parties agree to add this HTLC. If a staging request packet is
   received, then BOTH PARTIES will have it in their next Commitment. Nothing
   is guaranteed here, but violations are treated as immediate channel closure.
3. Signed and sent the Commitment Tx to the counterparty, one should now assume
   that there's a possibility that this HTLC will be boradcast on-chain.
4. Completed and Revoked, *counterparty* has included this in the Commitment
   they're broadcasting and revoked their prior state. This means the
   *Requeseter* can continue to take action, since the Commitment they have,
   the HTLC doesn't exist (no payment), and the *Responder* will broadcast with
   the payment to the *Responder*. However, the *Responder* cannot treat the
   HTLC as cleared.
5. Cleared. Both parties have signed and revoked. Responder can continue
   routing. Make sure it's included in *BOTH COMMITMENTS and ALL PREVIOUS
   REVOKED*
6. Staging Reject, removal request, tx rejected, begin flow to reject HTLC from
   other channels, can only be sent during the *pre-staging* state

In the event that an HTLC stays in "Completed and Revoked" and it is timed out,
and the counterparty refuses to add it into a new Commitment, the channel is
*closed out on-chain*. In other words, when checking which ones to send a
settle/timeout notification, do it for anything which is
ADD\_SIGNING\_AND\_REVOKING, *or* ADD\_COMPLETE (AND ALL OTHER PRE-COMPLETE
STAGES, e.g. in timeout or settlement).

As part of moving to any further stage, check if it's timed out.

If there is a request to stage and it's already staged, treat it as accepting.

When it has cleared and timed out, a timeout notification is sent.

HTLC ID numbers are uint64 and each counterparty is responsible to only make
sequential/incremental, and each party can only make evens/odds (odd channel
creation responder, evens channel creation initiator)

State is for *YOUR* signatures (what kind of action you need to do in the future)
ADD\_PRESTAGE
ADD\_STAGED
ADD\_SIGNING\_AND\_REVOKING
ADD\_COMPLETE
ADD\_REJECTED

Messages:
HTLCAddRequest: Request to add to staging
HTLCAddAccept: Add to staging (both parties have added when recv)
HTLCAddReject: Deny add to staging (both parties don't have in staging)

HTLC Settle (payment success)
-----------------------------

Requester Settle HTLC states (Fulfill HTLCs):
1. Pre-staged, don't know if the other person will agree to settle
2. Staged, both parties agree to settle this HTLC
3. Signed and sent Commitment Tx to the counterparty, there is now the
   possibility that the HTLC does not exist on-chain (of course, the Commitment
   includes the payment so there's no real loss of funds). In the event that it
   does not complete past this step, then one *must* close out on-chain as if
   it was never staged/signed in the first place and the counterparty went
   offline.
4. Both parties have signed and revoked, the settlement is complete (there is
   no intermediate step of Revoked because this is only reliable and actionable
   if BOTH PARTIES have updated their settlement state).

This has one less state because when adding, you're encumbering yourself. With
removing, both parties are potentially encumbered, so they cannot take action
until it's fully settled.

State is for *your* signatures
SETTLE\_PRESTAGE
SETTLE\_STAGED
SETTLE\_SIGNING\_AND\_REVOKING
SETTLE\_COMPLETE

Message:
HTLCSettleRequest: Request to add to staging the removal from Commitment.
HTLCSettleAccept: Add to staging the removal from Commitment.
(There is no HTLCSettleReject as the counterparty should immediately close out
or at worst ignore if it's getting garbage requests)

Timeout (falure/refund)
-----------------------

Requester Timeout HTLC States:
1. Pre-staged
2. Staged, both parties agree to time out the HTLC and refund the money
3. Signe dnad sent commitment to the counterparty, there is now the possibility
   that the transaction will no longer exist on-chain (of course, they can be
   redeemed either way). In the even that it does not complete past this step,
   then one *must* close out on-chain as if it was never staged/signed in the
   first place adn the counterparty was offline.
4. Both parties have signed and revoked, the settlement is complete (there is no
   intermediate step of Revoked because there is only reliable and actionable if
   BOTH PARTIES have updated their settlement state).

Similar to HTLC Settlement, there is one less state.

State is for *your* signatures
TIMEOUT\_PRESTAGE
TIMEOUT\_STAGED
TIMEOUT\_SIGNING\_AND\_REVOKING
TIMEOUT\_COMPLETE


Example
-------

Adding a single HTLC process:
1. Requester flags as pre-staged, and sends an "add requeset"
2. Responder decides whether to add. If they don't, they invalidate it. If they
   do, they send a message accepting the staging request. It is now marked as
   staged on both sides and is ready to be accepted into a Commitment.
3. When a party wants to update with a new Commitment, they send a new signed
   Commitment, this includes data that the HTLC is part of it. Let's say it's
   the Requester that sends this new Commitment. As a result, the HTLC is
   marked *BY THE RESPONDER* as Signed. It's only when the Responder includes a
   transaction including the new HTLC in a new Commitment that the Requester
   marks it as Signed.
4. Upon the Responder receiving the new Commitment, they send the revocation
   for the old Commitment, and commit to broadcasting only the new one.
5. The *Requester* marks the HTLC as complete, but the *Responder* waits until
   they receive a Commitment (and the old one is revoked) before marking it as
   complete on the Responder's end.
6. When both parties have the new Commitments and the old ones are revoked,
   then the HTLC is marked as complete

The two Commitment Transactions may not be completely in sync, but that's OK!
What's added in both (and removed in both) are regarded as valid and locked-in.
If it's only added to one, then it's regarded as in-transit and can go either
way.

The behavior is to sign after all additions/removals/cancellations, but there
may be multiple in the staging buffer.

Each party has their own revocation height (*for the other party to use*), and
they may be different.
