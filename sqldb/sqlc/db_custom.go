package sqlc

import (
	"fmt"
	"strings"
)

// GetTx returns the underlying DBTX (either *sql.DB or *sql.Tx) used by the
// Queries struct.
func (q *Queries) GetTx() DBTX {
	return q.db
}

// makeQueryParams generates a string of query parameters for a SQL query. It is
// meant to replace the `?` placeholders in a SQL query with numbered parameters
// like `$1`, `$2`, etc. This is required for the sqlc /*SLICE:<field_name>*/
// workaround. See scripts/gen_sqlc_docker.sh for more details.
func makeQueryParams(numTotalArgs, numListArgs int) string {
	if numListArgs == 0 {
		return ""
	}

	var b strings.Builder

	// Pre-allocate a rough estimation of the buffer size to avoid
	// re-allocations. A parameter like $1000, takes 6 bytes.
	b.Grow(numListArgs * 6)

	diff := numTotalArgs - numListArgs
	for i := 0; i < numListArgs; i++ {
		if i > 0 {
			// We don't need to check the error here because the
			// WriteString method of strings.Builder always returns
			// nil.
			_, _ = b.WriteString(",")
		}

		// We don't need to check the error here because the
		// Write method (called by fmt.Fprintf) of strings.Builder
		// always returns nil.
		_, _ = fmt.Fprintf(&b, "$%d", i+diff+1)
	}

	return b.String()
}

// ChannelAndNodes is an interface that provides access to a channel and its
// two nodes.
type ChannelAndNodes interface {
	// Channel returns the GraphChannel associated with this interface.
	Channel() GraphChannel

	// Node1 returns the first GraphNode associated with this channel.
	Node1() GraphNode

	// Node2 returns the second GraphNode associated with this channel.
	Node2() GraphNode
}

// Channel returns the GraphChannel associated with this interface.
//
// NOTE: This method is part of the ChannelAndNodes interface.
func (r GetChannelsByPolicyLastUpdateRangeRow) Channel() GraphChannel {
	return r.GraphChannel
}

// Node1 returns the first GraphNode associated with this channel.
//
// NOTE: This method is part of the ChannelAndNodes interface.
func (r GetChannelsByPolicyLastUpdateRangeRow) Node1() GraphNode {
	return r.GraphNode
}

// Node2 returns the second GraphNode associated with this channel.
//
// NOTE: This method is part of the ChannelAndNodes interface.
func (r GetChannelsByPolicyLastUpdateRangeRow) Node2() GraphNode {
	return r.GraphNode_2
}

// ChannelAndNodeIDs is an interface that provides access to a channel and its
// two node public keys.
type ChannelAndNodeIDs interface {
	// Channel returns the GraphChannel associated with this interface.
	Channel() GraphChannel

	// Node1Pub returns the public key of the first node as a byte slice.
	Node1Pub() []byte

	// Node2Pub returns the public key of the second node as a byte slice.
	Node2Pub() []byte
}

// Channel returns the GraphChannel associated with this interface.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsBySCIDWithPoliciesRow) Channel() GraphChannel {
	return r.GraphChannel
}

// Node1Pub returns the public key of the first node as a byte slice.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsBySCIDWithPoliciesRow) Node1Pub() []byte {
	return r.GraphNode.PubKey
}

// Node2Pub returns the public key of the second node as a byte slice.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsBySCIDWithPoliciesRow) Node2Pub() []byte {
	return r.GraphNode_2.PubKey
}

// Node1 returns the first GraphNode associated with this channel.
//
// NOTE: This method is part of the ChannelAndNodes interface.
func (r GetChannelsBySCIDWithPoliciesRow) Node1() GraphNode {
	return r.GraphNode
}

// Node2 returns the second GraphNode associated with this channel.
//
// NOTE: This method is part of the ChannelAndNodes interface.
func (r GetChannelsBySCIDWithPoliciesRow) Node2() GraphNode {
	return r.GraphNode_2
}

// Channel returns the GraphChannel associated with this interface.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsByOutpointsRow) Channel() GraphChannel {
	return r.GraphChannel
}

// Node1Pub returns the public key of the first node as a byte slice.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsByOutpointsRow) Node1Pub() []byte {
	return r.Node1Pubkey
}

// Node2Pub returns the public key of the second node as a byte slice.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsByOutpointsRow) Node2Pub() []byte {
	return r.Node2Pubkey
}

// Channel returns the GraphChannel associated with this interface.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsBySCIDRangeRow) Channel() GraphChannel {
	return r.GraphChannel
}

// Node1Pub returns the public key of the first node as a byte slice.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsBySCIDRangeRow) Node1Pub() []byte {
	return r.Node1PubKey
}

// Node2Pub returns the public key of the second node as a byte slice.
//
// NOTE: This method is part of the ChannelAndNodeIDs interface.
func (r GetChannelsBySCIDRangeRow) Node2Pub() []byte {
	return r.Node2PubKey
}
