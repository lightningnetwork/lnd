# Setting up zero-conf channels in LND

Zero-conf channels are channels that do not require confirmations to be used. Because of this,
the fundee must trust the funder to not double-spend the channel and steal the balance of the
channel.

## Startup

Two options must be specified either on the command line or in the config file:
- `protocol.option-scid-alias`
- `protocol.zero-conf`

If one of these is missing, zero-conf channels won't work. This applies for both the funder
and fundee.

## Opening the channel

The channel open flow is slightly different, so there are new requirements for the
initiator and responder in the funding flow.

### Initiator requirements

When opening the channel, the initiator must ensure that the `anchors` channel_type is
enabled. Additionally, the initiator must specify on the `openchannel` command line call
or the corresponding RPC that the zero-conf flag is set.

### Responder requirements

The responder flow is different from the initiator's. If the responder has not specified
a ChannelAcceptor, then ALL open channel requests will be failed regardless if they are
zero-conf or not. The ChannelAcceptor RPC will give the responder information on whether
the initiator is requesting a zero-conf channel via the channel type.

If the responder has specified a ChannelAcceptor and the funder has set the zero-conf
channel type, then the responder should set the ZeroConf flag to true if they wish to
accept it and false otherwise. If ZeroConf is true, then MinAcceptDepth should be zero.

It is possible for the responder to set the ZeroConf flag to true even when the funder
did not specify the zero-conf channel type. This will only create a zero-conf channel if
the funder is a non-LND node that supports this behavior. This is for compatibility with
LDK nodes. In this case, the responder must know that the funder is using an implementation
that supports this behavior (like LDK). The scid-alias feature bit must also have been
negotiated.

## Updated RPC calls

The `listchannels` and `closedchannels` RPC calls have been updated. They now include a
list of all aliases that the channel has used. The `ChanId` in the response is the
confirmed SCID for non-zero-conf channels. For zero-conf channels, the `ChanID` is the
first alias used in the channel. The RPCs will also return the confirmed SCID for
zero-conf channels in a separate field.

A new RPC `listaliases` has been introduced. It returns a set of mappings from one SCID
to a list of SCIDS. The key is the confirmed SCID for non-zero-conf channels. For
zero-conf channels, it is the first alias used in the channel. The values are the list
of all aliases ever used in the channel (including the key for zero-conf channels). This
information can be cross-referenced with the output of `listchannels` and `closedchannels`
to determine what channel a particular alias belongs to.