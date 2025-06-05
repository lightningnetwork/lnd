# Sphinx Onion Routing in Lightning Network

The Lightning Network uses a Sphinx-based onion message protocol to send
messages across the Lightning Network. These messages have the property that a
node which is part of such a message and for example forwards such an onion
message cannot learn about the destination of this whole packet. It only knows
the predecessor and the successor of the message. In other words, it only knows
where the message came from and where it needs to be forwarded. This makes the
message protocol of the Lightning Network very private. Only the sender
(creator of the whole onion packet) knows the whole route of the packet. Also,
the receiver has no idea from which node the message originated.

This diagram illustrates how Sphinx onion routing works in the Lightning
Network, showing the privacy properties at each hop:

```ascii
Alice (Sender)
    |
    | Knows Full Route: Alice → Bob → Carol → David → Eve
    |
    v
Bob (Hop 1)
    |
    | Knows: Alice → Carol
    |
    v
Carol (Hop 2)
    |
    | Knows: Bob → David
    |
    v
David (Hop 3)
    |
    | Knows: Carol → Eve
    |
    v
Eve (Receiver)
    |
    | Knows: From David
    |
    v

Privacy Properties:
- Each hop only knows its immediate neighbors
- Receiver doesn't know the original sender
- Only sender knows the complete route
- Each hop peels one layer of the onion
```

## Privacy Properties

1. **Hop Privacy**: Each intermediate node only knows:
   - The previous hop (where the packet came from)
   - The next hop (where to forward the packet)
   - Cannot see the full route or final destination

2. **Sender Privacy**: Only the sender (Alice) knows:
   - The complete route
   - All intermediate nodes
   - The final destination

3. **Receiver Privacy**: The receiver (Eve) only knows:
   - The immediate previous hop (David)
   - Cannot determine the original sender

4. **Onion Encryption**: Each hop peels one layer of encryption, revealing only
   the next hop's information


The detailed mechanics are described in [BOLT 04](https://github.com/lightning/bolts/blob/master/04-onion-routing.md)


## Replay Protection

Replaying (resending) onion packets into the network can compromise the
privacy guarantees promised by the protocol. So it is crucial for node
participants to not forward replayed onion packets to guard the privacy of all
network participants. Compared to the original Sphinx protocol, the Lightning
Network has an improved replay protection in place especially when it comes to
forwarding HTLCs, which are different from Onion Messages introduced later on
because they lock a payment to the particular onion packet. Therefore, sending
HTLCs packets comes with a cost. Moreover, every HTLC has an expiry date, also
called CLTV (absolute locktime), which prevents the replay of packets that have
already expired. In addition to the CLTV expiry of a packet, every HTLC onion
packet commits to the payment hash in the HMAC of the message (to be precise,
the associated data), so this prevents an attacker from attaching an old onion
packet to a new payment hash, which now also comes with the risk for the
attacker that he does not only have to lock funds when replaying an onion
packet but he also risks that the next node settles the HTLC because it already
knows the preimage of the HTLC. Although the attack comes with a high cost,
Lightning implementations should prevent replayed onion packets from
propagating through the network to safeguard the privacy for every network
participant. An attacker could, for example, in hindsight access onion packets
from several highly connected nodes and re-inject it into the network and
potentially unblind receivers of payments which already happened in the past.

### Replay Protection in LND

For that reason the LND lightning implementation implements the reply protection
of those  replayed onion packets. In LND there are two interchanging DB names
which save information of those onion packets to prevent replays from happening.
They are called `Sphinx-Replay-DB` or `Decayed-Log-DB`. Currently LND does only
implement onion messages which are tied to payments (HTLCs) therefore as
discussed earlier they decay after the abolute locktime of the HTLC expires and
therefore can be garbage collected because they will not be forwarded by nodes
anyways because they would risk losing funds.

Where is the sphinx replay protection stored for the different backends:

1. When running LND with the BBolt backend the db is called: `sphinxreplay.db`

2. For Postgres the table is called: `decayedlogdb_kv`

3. For sqlite the table is called `decayedlogdb_kv` and is part of the
   channel.sqlite file.

#### What happens if LND encounters a replayed onion HTLC packet?

When LND encounters a replay it will reject the HTLC it will not signal a
specific error that a replay occurred which would reveal that the node was
indeed part of the route in the onion packet. LND will reject the HTLC and 
respond with the message `invalid_onion_version` see also
[BOLT 04](https://github.com/lightning/bolts/blob/master/04-onion-routing.md)