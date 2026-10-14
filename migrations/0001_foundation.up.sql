-- 0001 foundation: UTF8 check, schema orvex and the shared append-only trigger function (LBT-10 spec P-1, P-11, P-13)

DO $$
BEGIN
    IF current_setting('server_encoding') <> 'UTF8' THEN
        RAISE EXCEPTION 'orvex requires a UTF8 database' USING ERRCODE = 'OX007';
    END IF;
END
$$;

CREATE SCHEMA orvex;

COMMENT ON SCHEMA orvex IS 'Orvex system of record: rounds, admissions, chain records, ledger and outbox (ADR-005).';

-- forbid_mutation serves the row-level <table>_append_only and <table>_no_delete
-- triggers and the statement-level <table>_no_truncate triggers (P-13).
CREATE FUNCTION orvex.forbid_mutation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    RAISE EXCEPTION 'orvex.% is append-only: % rejected', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'OX001';
END
$$;
