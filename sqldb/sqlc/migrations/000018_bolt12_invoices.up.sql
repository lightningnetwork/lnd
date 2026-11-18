-- bolt12_invoices holds the BOLT 12 data of a received invoice. The shared
-- invoices table stays protocol agnostic, so only BOLT 12 queries touch this
-- table. The receiver keeps no signed invoice string after stateless
-- settlement, so this row holds every BOLT 12 fact it needs later. A row also
-- marks its invoice as a BOLT 12 invoice.
CREATE TABLE IF NOT EXISTS bolt12_invoices (
    -- The invoice this data belongs to. The row is deleted with it.
    invoice_id BIGINT PRIMARY KEY REFERENCES invoices(id) ON DELETE CASCADE,

    -- The offer that created the invoice. NULL for an invoice that answers
    -- no offer.
    offer_id BIGINT REFERENCES offers(id),

    -- The invoice envelope that the payment carried. It is the
    -- authoritative source of the BOLT 12 order data of the invoice.
    envelope BLOB NOT NULL
);

CREATE INDEX IF NOT EXISTS bolt12_invoices_offer_id_idx
ON bolt12_invoices(offer_id);
