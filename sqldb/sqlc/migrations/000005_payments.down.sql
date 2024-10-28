-- Drop htlc_settle_info index and table.
DROP INDEX IF EXISTS htlc_settle_info_htlc_attempt_id_idx;
DROP TABLE IF EXISTS htlc_settle_info;

-- Drop htlc_fail_info index and table.
DROP INDEX IF EXISTS htlc_fail_info_htlc_attempt_id_idx;
DROP TABLE IF EXISTS htlc_fail_info;

-- Drop htlc_fail_reason_types index and table.
DROP INDEX IF EXISTS htlc_fail_reason_types_htlc_fail_reason_type_idx;
DROP TABLE IF EXISTS htlc_fail_reason_types;

-- Drop blinded_data index and table.
DROP INDEX IF EXISTS blinded_data_hop_id_idx;
DROP TABLE IF EXISTS blinded_data;

-- Drop mpp_record index and table.
DROP INDEX IF EXISTS mpp_record_payment_addr_idx;
DROP TABLE IF EXISTS mpp_record;

-- Drop amp_record index and table.
DROP INDEX IF EXISTS amp_record_set_id_idx;
DROP TABLE IF EXISTS amp_record;

-- Drop hop index and table.
DROP INDEX IF EXISTS hop_route_id_idx;
DROP TABLE IF EXISTS hop;

-- Drop route index and table.
DROP INDEX IF EXISTS route_htlc_attempt_id_idx;
DROP TABLE IF EXISTS route;

-- Drop htlc_attempt index and table.
DROP INDEX IF EXISTS htlc_attempt_payment_id_idx;
DROP TABLE IF EXISTS htlc_attempt;

-- Drop first_hop_custom_records index and table.
DROP INDEX IF EXISTS first_hop_custom_records_payment_id_idx;
DROP TABLE IF EXISTS first_hop_custom_records;

-- Drop mpp_state index and table.
DROP INDEX IF EXISTS mpp_state_payment_id_idx;
DROP TABLE IF EXISTS mpp_state;

-- Drop payment_status_types index and table.
DROP INDEX IF EXISTS payment_status_types_name_idx;
DROP TABLE IF EXISTS payment_status_types;

-- Drop payments index and table.
DROP INDEX IF EXISTS payments_payment_status_idx;
DROP INDEX IF EXISTS payments_payment_type_idx;
DROP INDEX IF EXISTS payments_created_at_idx;
DROP TABLE IF EXISTS payments;
