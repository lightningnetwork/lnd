-- amp_sub_invoices holds all AMP sub-invoices that belong to a given parent
-- invoice.
CREATE TABLE IF NOT EXISTS amp_sub_invoices (
    -- The set id identifying the payment. 
    set_id BLOB PRIMARY KEY,

    -- The state of this amp payment. This matches the state for all the htlcs 
    -- belonging to this set id. The A in AMP stands for Atomic.
    state SMALLINT NOT NULL,

    -- Timestamp of when the first htlc for this payment was accepted.
    created_at TIMESTAMP NOT NULL,

    -- When the invoice was settled.
    settled_at TIMESTAMP,

    -- If settled, the index is set to the current_value+1 of the settle_index
    -- seuqence in the invoice_sequences table. If not settled, then it is set
    -- to NULL.
    settle_index BIGINT,

    -- The id of the parent invoice this AMP invoice belongs to.
    invoice_id BIGINT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,

    -- A unique constraint on the set_id and invoice_id is needed to ensure that
    -- we don't have two AMP invoices for the same invoice.
    UNIQUE (set_id, invoice_id)
);

CREATE INDEX IF NOT EXISTS amp_sub_invoices_invoice_id_idx ON amp_sub_invoices(invoice_id);

-- amp_invoice_htlcs contains the complementary information for an htlc related 
-- to an AMP invoice.
CREATE TABLE IF NOT EXISTS amp_sub_invoice_htlcs (
    -- The invoice id this entry is related to.
    invoice_id BIGINT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,

    -- The set id identifying the payment this htlc belongs to.
    set_id BLOB NOT NULL REFERENCES amp_sub_invoices(set_id) ON DELETE CASCADE,

    -- The id of the htlc this entry blongs to.
    htlc_id BIGINT NOT NULL REFERENCES invoice_htlcs(id) ON DELETE CASCADE,

    -- The root share for this amp htlc.
    root_share BLOB NOT NULL,
    
    -- The child index for this amp htlc.
    child_index BIGINT NOT NULL,

    -- The htlc-level payment hash. An AMP htlc will carry a different payment
    -- hash from the invoice it might be satisfying. They are needed to ensure
    -- that we reconstruct the preimage correctly.
    hash BLOB NOT NULL,
    
    -- The HTLC-level preimage that satisfies the AMP htlc's Hash.
    -- The preimage will be derived either from secret share reconstruction of 
    -- the shares in the AMP payload.
    preimage BLOB 
);

CREATE INDEX IF NOT EXISTS amp_htlcs_invoice_id_idx ON amp_sub_invoice_htlcs(invoice_id);
CREATE INDEX IF NOT EXISTS amp_htlcs_set_id_idx ON amp_sub_invoice_htlcs(set_id);
CREATE INDEX IF NOT EXISTS amp_htlcs_htlc_id_idx ON amp_sub_invoice_htlcs(htlc_id);

