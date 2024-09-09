-- blinded_paths contains information about blinded paths included in the
-- associated invoice.
CREATE TABLE IF NOT EXISTS blinded_paths(
    -- The id of the blinded path
    id BIGINT PRIMARY KEY,

    -- invoice_id is the reference to the invoice this blinded_path was created
    -- for.
    invoice_id BIGINT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,

    -- last_ephemeral_pub is the public key of the last ephemeral blinding
    -- point of this path.
    last_ephemeral_pub BLOB NOT NULL UNIQUE,

    -- session_key is the private key used for the first ephemeral blinding
    -- key of this path.
    session_key BLOB NOT NULL,

    -- introduction_node is the public key of the first hop of the path.
    introduction_node BLOB NOT NULL,

    -- amount_msat is the total amount in millisatoshis expected to be
    -- forwarded along this path.
    amount_msat BIGINT NOT NULL
);

-- blinded_paths_hops holds information about a specific hop of a blinded path in
-- blinded_paths.
CREATE TABLE IF NOT EXISTS blinded_path_hops(
    -- blinded_path_id is the reference to the blinded_path_id this
    -- blinded_path_hop is part of.
    blinded_path_id BIGINT NOT NULL REFERENCES blinded_paths(id) ON DELETE CASCADE,

    -- hop_index is the index of this hop along the associated blinded path.
    hop_index BIGINT NOT NULL,

    -- channel_id is the ID of the channel that connects this hop to the previous one.
    channel_id BIGINT NOT NULL,

    -- node_pub_key is the public key of the node of this hop
    node_pub_key BLOB NOT NULL,

    -- amount_to_fwd is the amount that this hop was instructed to forward.
    amount_to_fwd BIGINT NOT NULL,

    -- The hop_index is unique per path.
    UNIQUE (blinded_path_id, hop_index)
);

CREATE INDEX IF NOT EXISTS blinded_path_hops_path_id_idx ON blinded_path_hops(blinded_path_id);
