DROP INDEX IF EXISTS invoice_htlc_custom_records_htlc_id_idx;
DROP TABLE IF EXISTS invoice_htlc_custom_records;

DROP INDEX IF EXISTS invoice_htlc_invoice_id_idx;
DROP TABLE IF EXISTS invoice_htlcs;

DROP INDEX IF EXISTS invoice_feature_invoice_id_idx;
DROP TABLE IF EXISTS invoice_features;

DROP INDEX IF EXISTS invoices_settled_at_idx;
DROP INDEX IF EXISTS invoices_created_at_idx;
DROP INDEX IF EXISTS invoices_state_idx;
DROP INDEX IF EXISTS invoices_payment_addr_idx;
DROP INDEX IF EXISTS invoices_preimage_idx;
DROP INDEX IF EXISTS invoices_hash_idx;
DROP TABLE IF EXISTS invoices;
