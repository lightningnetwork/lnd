-- sequences contains all sequences used for invoices.
CREATE TABLE IF NOT EXISTS invoice_sequences (
    name TEXT PRIMARY KEY,
    current_value BIGINT NOT NULL
);

-- Initialize a sequence for the settled index used to track invoice settlement
-- to remain compatible with the legacy channeldb implementation.
INSERT INTO invoice_sequences (name, current_value) 
VALUES ('settle_index', 0) 
    ON CONFLICT (name) DO NOTHING;

-- invoices table contains all the information shared by all the invoice types. 
CREATE TABLE IF NOT EXISTS invoices (
    -- The id of the invoice. Translates to the AddIndex.
    id INTEGER PRIMARY KEY,

    -- The hash for this invoice. The invoice hash will always identify that 
    -- invoice.
    hash BLOB NOT NULL UNIQUE,

    -- The preimage for the hash in this invoice. Some invoices may have this 
    -- field empty, like unsettled hodl invoices or AMP invoices.
    preimage BLOB,
    
    -- If settled, the index is set to the current_value+1 of the settle_index
    -- seuqence in the invoice_sequences table. If not settled, then it is set
    -- to NULL.
    settle_index BIGINT,

    -- When the invoice was settled.
    settled_at TIMESTAMP,

    -- An optional memo to attach along with the invoice. 
    memo TEXT,

    -- The amount of the invoice in millisatoshis.
    amount_msat BIGINT NOT NULL, 

    -- Delta to use for the time-lock of the CLTV extended to the final hop.
    -- BOLT12 invoices will have this field set to NULL.
    cltv_delta INTEGER,

    -- The time before this invoice expiries, in seconds.
    expiry INTEGER NOT NULL,

    -- A randomly generated value include in the MPP record by the sender to 
    -- prevent probing of the receiver.
    -- This field corresponds to the `payment_secret` specified in BOLT 11.
    payment_addr BLOB UNIQUE,

    -- The encoded payment request for this invoice. Some invoice types may 
    -- not have leave this empty, like Keysends.
    payment_request TEXT,

    -- Holds the hash of the payment request. This field is used to ensure that
    -- there are no duplicates in the database. This trick is needed because
    -- PostgreSQL has a limitiation of 2712 bytes for unique index rows.
    payment_request_hash BLOB UNIQUE,

    -- The invoice state.
    state SMALLINT NOT NULL,

    -- The accumulated value of all the htlcs settled for this invoice.
    amount_paid_msat BIGINT NOT NULL,

    -- This field will be true for AMP invoices.
    is_amp BOOLEAN NOT NULL,

    -- This field will be true for hodl invoices, independently of they being
    -- settled or not.
    is_hodl BOOLEAN NOT NULL,

    -- This field will be true for keysend invoices.
    is_keysend BOOLEAN NOT NULL,

    -- Timestamp of when this invoice was created.
    created_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS invoices_hash_idx ON invoices(hash);
CREATE INDEX IF NOT EXISTS invoices_preimage_idx ON invoices(preimage);
CREATE INDEX IF NOT EXISTS invoices_payment_addr_idx ON invoices(payment_addr);
CREATE INDEX IF NOT EXISTS invoices_state_idx ON invoices(state);
CREATE INDEX IF NOT EXISTS invoices_created_at_idx ON invoices(created_at);
CREATE INDEX IF NOT EXISTS invoices_settled_at_idx ON invoices(settled_at);

-- invoice_features contains the feature bits of an invoice.
CREATE TABLE IF NOT EXISTS invoice_features (
    -- The feature bit.
    feature INTEGER NOT NULL,

    -- The invoice id this feature belongs to.
    invoice_id BIGINT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,

    -- The feature bit is unique per invoice.
    UNIQUE (feature, invoice_id)
);

CREATE INDEX IF NOT EXISTS invoice_feature_invoice_id_idx ON invoice_features(invoice_id);

-- invoice_htlcs contains the information of a htlcs related to one of the node 
-- invocies. 
CREATE TABLE IF NOT EXISTS invoice_htlcs (
    -- The id for this htlc. Used in foreign keys instead of the 
    -- htlc_id/chan_id combination.
    id INTEGER PRIMARY KEY,

    -- Short chan id indicating the htlc's origin. uint64 stored as text.
    chan_id TEXT NOT NULL, 

    -- The uint64 htlc id. This field is a counter so it is safe to store it as 
    -- int64 in the database. The application layer must check that there is no 
    -- overflow when storing/loading this column.
    htlc_id BIGINT NOT NULL,

    -- The htlc's amount in millisatoshis.
    amount_msat BIGINT NOT NULL,

    -- The total amount expected for the htlcs in a multi-path payment.
    total_mpp_msat BIGINT, 

    -- The block height at which this htlc was accepted.
    accept_height INTEGER NOT NULL,

    -- The timestamp at which this htlc was accepted.
    accept_time TIMESTAMP NOT NULL,

    -- The block height at which this htlc expiries. 
    expiry_height INTEGER NOT NULL,

    -- The htlc state.
    state SMALLINT NOT NULL,

    -- Timestamp of when this htlc was resolved (settled/canceled).
    resolve_time TIMESTAMP,

    -- The id of the invoice this htlc belongs to.
    invoice_id BIGINT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,

    -- The htlc_id and chan_id identify the htlc.
    UNIQUE (chan_id, htlc_id)
);

CREATE INDEX IF NOT EXISTS invoice_htlc_invoice_id_idx ON invoice_htlcs(invoice_id);

-- invoice_htlc_custom_records stores the custom key/value pairs that 
-- accompanied an htlc.
CREATE TABLE IF NOT EXISTS invoice_htlc_custom_records (
    -- The custom type identifier for this record.
    -- The range of values for custom records key is defined in BOLT 01.
    key BIGINT NOT NULL,

    -- The custom value for this record.
    value BLOB NOT NULL,

    -- The htlc id this record belongs to. 
    htlc_id BIGINT NOT NULL REFERENCES invoice_htlcs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS invoice_htlc_custom_records_htlc_id_idx ON invoice_htlc_custom_records(htlc_id);

