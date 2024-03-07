-- invoice_event_types stores the different types of invoice events. 
CREATE TABLE IF NOT EXISTS invoice_event_types(
    id INTEGER PRIMARY KEY,

    description TEXT NOT NULL
);

-- invoice_event_types defines the different types of invoice events.
INSERT INTO invoice_event_types (id, description) 
VALUES 
    -- invoice_created is the event type used when an invoice is created.
    (0, 'invoice_created'), 
    -- invoice_canceled is the event type used when an invoice is canceled.
    (1, 'invoice_canceled'), 
    -- invoice_settled is the event type used when an invoice is settled.
    (2, 'invoice_settled'),
    -- setid_created is the event type used when an AMP sub invoice
    -- corresponding to the set_id is created.
    (3, 'setid_created'),
    -- setid_canceled is the event type used when an AMP sub invoice
    -- corresponding to the set_id is canceled.
    (4, 'setid_canceled'),
    -- setid_settled is the event type used when an AMP sub invoice 
    -- corresponding to the set_id is settled.
    (5, 'setid_settled');


-- invoice_events stores all major events related to the node's invoices and
-- AMP sub invoices. This table can be used to create a historical view of what
-- happened to the node's invoices.
CREATE TABLE IF NOT EXISTS invoice_events (
    id BIGINT PRIMARY KEY,

    -- added_at is the timestamp when this event was added.
    added_at TIMESTAMP NOT NULL,

    -- event_type is the type of this event.
    event_type INTEGER NOT NULL REFERENCES invoice_event_types(id) ON DELETE CASCADE,

    -- invoice_id is the reference to the invoice this event was added for.
    invoice_id BIGINT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,

    -- set_id is the reference to the AMP sub invoice this event was added for.
    -- May be NULL if the event is not related to an AMP sub invoice.
    set_id BLOB REFERENCES amp_sub_invoices(set_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS invoice_events_added_at_idx ON invoice_events(added_at);
CREATE INDEX IF NOT EXISTS invoice_events_event_type_idx ON invoice_events(event_type);
CREATE INDEX IF NOT EXISTS invoice_events_invoice_id_idx ON invoice_events(invoice_id);
CREATE INDEX IF NOT EXISTS invoice_events_set_id_idx ON invoice_events(set_id);
