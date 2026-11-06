-- offers stores long-lived BOLT 12 offer templates. Each offer can generate
-- many invoices over its lifetime.
CREATE TABLE IF NOT EXISTS offers (
    -- Primary key for the offer record.
    id INTEGER PRIMARY KEY,

    -- The SHA256 hash of the TLV-encoded offer, used as a unique external
    -- identifier. 32 bytes.
    hash BLOB NOT NULL UNIQUE,

    -- The full bech32-encoded offer string (lno1...). This is the
    -- authoritative source for all offer fields.
    encoded TEXT NOT NULL,

    -- Whether the offer has been administratively disabled. A disabled
    -- offer rejects new invoice requests.
    is_disabled BOOLEAN NOT NULL DEFAULT FALSE,

    -- Timestamp of when this offer was created.
    created_at TIMESTAMP NOT NULL
);
