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

Close Channel
-------------
One party unilaterally sends their sig and fee amount to the other party. No
further channel updates are possible. In the future, we might include HTLCs in
the outputs, but for now, we're assuming *all* HTLCs are cleared out.

Close Channel Complete
----------------------
Returns the Txid and sig as a courtesy. The counterparty might not send this if
they're being non-cooperative.

