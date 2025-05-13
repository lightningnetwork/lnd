DROP INDEX IF EXISTS amp_htlcs_htlc_id_idx;
DROP INDEX IF EXISTS amp_htlcs_invoice_id_idx;
DROP INDEX IF EXISTS amp_htlcs_set_id_idx;
DROP TABLE IF EXISTS amp_invoice_htlcs;

DROP INDEX IF EXISTS amp_invoice_payments_invoice_id_idx;
DROP TABLE IF EXISTS amp_invoice_payments;

