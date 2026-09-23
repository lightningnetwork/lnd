package chainntnfs

import "fmt"

// CheckSetInvariants verifies the lifetime invariants of the TxNotifier's
// request sets and reorg indexes. A set without subscribers must be kept alive
// by a pending historical scan or by details in the reorg index, since nothing
// else would ever remove it. Every reorg index entry must refer to a live set
// and lie inside the reorg safety window, since ConnectTip only prunes the
// entry at the window's edge.
func (n *TxNotifier) CheckSetInvariants() error {
	n.Lock()
	defer n.Unlock()

	for confRequest, confSet := range n.confNotifications {
		if len(confSet.ntfns) == 0 && confSet.pendingRescans == 0 &&
			!n.confDetailsIndexed(confRequest, confSet) {

			return fmt.Errorf("orphaned confirmation set for %v",
				confRequest)
		}
	}
	for height, confRequests := range n.confsByInitialHeight {
		if height+n.reorgSafetyLimit <= n.currentHeight {
			return fmt.Errorf("confirmation index at height %d "+
				"outside reorg window at tip %d", height,
				n.currentHeight)
		}
		for confRequest := range confRequests {
			if _, ok := n.confNotifications[confRequest]; !ok {
				return fmt.Errorf("confirmation index at "+
					"height %d refers to missing set %v",
					height, confRequest)
			}
		}
	}

	for spendRequest, spendSet := range n.spendNotifications {
		if len(spendSet.ntfns) == 0 && spendSet.pendingRescans == 0 &&
			!n.spendDetailsIndexed(spendRequest, spendSet) {

			return fmt.Errorf("orphaned spend set for %v",
				spendRequest)
		}
	}
	for height, spendRequests := range n.spendsByHeight {
		if height+n.reorgSafetyLimit <= n.currentHeight {
			return fmt.Errorf("spend index at height %d outside "+
				"reorg window at tip %d", height,
				n.currentHeight)
		}
		for spendRequest := range spendRequests {
			if _, ok := n.spendNotifications[spendRequest]; !ok {
				return fmt.Errorf("spend index at height %d "+
					"refers to missing set %v", height,
					spendRequest)
			}
		}
	}

	return nil
}
