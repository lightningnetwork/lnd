-- invoice_event_types stores the different types of events that can be emitted 
-- for invoices.
CREATE TABLE IF NOT EXISTS invoice_event_types(
    id INTEGER PRIMARY KEY,

    description TEXT NOT NULL
);

-- invoice_events stores all events related to the node invoices.
CREATE TABLE IF NOT EXISTS invoice_events (
    id INTEGER PRIMARY KEY,

    -- created_at is the creation time of this event.
    created_at TIMESTAMP NOT NULL,

    -- invoice_id is the reference to the invoice this event was emitted for.
    invoice_id INTEGER NOT NULL REFERENCES invoices(id),

    -- htlc_id is the reference to the htlc this event was emitted for, may be 
    -- null.
    htlc_id BIGINT REFERENCES invoice_htlcs(htlc_id),

    -- set_id is the reference to the set_id this event was emitted for, may be
    -- null.
    set_id BLOB NOT NULL REFERENCES amp_invoice_payments(set_id),

    -- event_type is the type of this event.
    event_type INTEGER NOT NULL REFERENCES invoice_event_types(id),

    -- event_metadata is a TLV encoding any relevant information for this kind 
    -- of events.
    event_metadata BLOB
);

CREATE INDEX IF NOT EXISTS invoice_events_created_at_idx ON invoice_events(created_at);
CREATE INDEX IF NOT EXISTS invoice_events_invoice_id_idx ON invoice_events(invoice_id);
CREATE INDEX IF NOT EXISTS invoice_events_htlc_id_idx ON invoice_events(htlc_id);
CREATE INDEX IF NOT EXISTS invoice_events_set_id_idx ON invoice_events(set_id);
CREATE INDEX IF NOT EXISTS invoice_events_event_type_idx ON invoice_events(event_type);


-- invoice_event_types defines the different types of events that can be emitted
-- for an invoice.
INSERT INTO invoice_event_types (id, description) 
VALUES 
    -- invoice_created is the event emitted when an invoice is created.
    (0, 'invoice_created'), 
    -- invoice_canceled is the event emitted when an invoice is canceled.
    (1, "invoice_canceled"), 
    -- invoice_settled is the event emitted when an invoice is settled.
    (2, "invoice_settled"),
    -- setid_created is the event emitted when the first htlc for the set_id is 
    -- received. 
    (3, "setid_created"),
    -- setid_canceled is the event emitted when the set_id is canceled.
    (4, "setid_canceled"),
    -- setid_settled is the event emitted when the set_id is settled.
    (5, "setid_settled");

