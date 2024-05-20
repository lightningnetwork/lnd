* Some new experimental [RPCs for managing SCID 
  aliases](https://github.com/lightningnetwork/lnd/pull/8509) were added under
  the routerrpc package. These methods allow manually adding and deleting scid 
  aliases locally to your node.
  > NOTE: these new RPC methods are marked as experimental 
        (`XAddLocalChanAliases` & `XDeleteLocalChanAliases`) and upon calling
        them the aliases will not be communicated with the channel peer. 
