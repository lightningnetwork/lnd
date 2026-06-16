-- The chain_params table stores chain-level properties of the database. It is
-- used to persist and validate chain-specific invariants across restarts, such
-- as which Bitcoin network the database was initialised for. The single_row
-- column is a boolean primary key that enforces exactly one row in the table.
CREATE TABLE IF NOT EXISTS chain_params (
    single_row BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (single_row),
    network    TEXT    NOT NULL
);
