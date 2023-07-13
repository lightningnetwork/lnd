DROP TABLE IF EXISTS invoice_events;

DROP INDEX IF EXISTS invoice_events_created_at_idx;
DROP INDEX IF EXISTS invoice_events_invoice_id_idx;
DROP INDEX IF EXISTS invoice_events_htlc_id_idx;
DROP INDEX IF EXISTS invoice_events_set_id_idx;
DROP INDEX IF EXISTS invoice_events_event_type_idx;

DROP TABLE IF EXISTS invoice_event_types;
