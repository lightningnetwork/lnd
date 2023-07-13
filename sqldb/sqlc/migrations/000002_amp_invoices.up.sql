-- amp_invoices_payments  
CREATE TABLE IF NOT EXISTS amp_invoice_payments (
    -- The set id identifying the payment. 
    set_id BLOB PRIMARY KEY,

    -- The state of this amp payment. This matches the state for all the htlcs 
    -- belonging to this set id. The A in AMP stands for Atomic.
    state SMALLINT NOT NULL,

    -- Timestamp of when the first htlc for this payment was accepted.
    created_at TIMESTAMP NOT NULL,

    -- If settled, the invoice payment related to this set id.
    settled_index INTEGER REFERENCES invoice_payments(id),

    -- The invoice id this set id is related to.
    invoice_id INTEGER NOT NULL REFERENCES invoices(id)
);

CREATE INDEX IF NOT EXISTS amp_invoice_payments_invoice_id_idx ON amp_invoice_payments(invoice_id);

-- amp_invoice_htlcs contains the complementary information for an htlc related 
-- to an AMP invoice.
CREATE TABLE IF NOT EXISTS amp_invoice_htlcs (
    -- The set id identifying the payment this htlc belongs to.
    set_id BLOB NOT NULL REFERENCES amp_invoice_payments(set_id),

    -- The id of the htlc this entry blongs to.
    htlc_id BIGINT NOT NULL REFERENCES invoice_htlcs(id),

    -- The invoice id this entry is related to.
    invoice_id INTEGER NOT NULL REFERENCES invoices(id),

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

CREATE INDEX IF NOT EXISTS amp_htlcs_set_id_idx ON amp_invoice_htlcs(set_id);
CREATE INDEX IF NOT EXISTS amp_htlcs_invoice_id_idx ON amp_invoice_htlcs(invoice_id);
CREATE INDEX IF NOT EXISTS amp_htlcs_htlc_id_idx ON amp_invoice_htlcs(htlc_id);

