-- 0008 execution_ledger: one posting per chained event and released credit holds at execution commit (LBT-12 spec E-10, E-11)

DROP INDEX orvex.ledger_postings_event_idx;

CREATE UNIQUE INDEX ledger_postings_event_key ON orvex.ledger_postings (round_id, event_sequence)
    WHERE event_sequence IS NOT NULL;

COMMENT ON INDEX orvex.ledger_postings_event_key IS 'At most one posting per chained domain event, so a bet settles at most once (ADR-005, ADR-001).';

-- check_record_holds_released runs at commit for every inserted record
-- (E-10, E-11): the transaction that executes an admission releases every
-- credit hold of that admission.
CREATE FUNCTION orvex.check_record_holds_released() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.sequence >= 1 AND EXISTS (
        SELECT 1
            FROM orvex.ledger_holds h
            WHERE h.round_id = NEW.round_id AND h.sequence = NEW.sequence
                AND NOT EXISTS (
                    SELECT 1
                        FROM orvex.ledger_hold_releases r
                        WHERE r.round_id = h.round_id AND r.sequence = h.sequence
                            AND r.account_id = h.account_id)) THEN
        RAISE EXCEPTION 'orvex.chain_records: record % of round % leaves a credit hold unreleased',
            NEW.sequence, NEW.round_id
            USING ERRCODE = 'OX009';
    END IF;
    RETURN NULL;
END
$$;

COMMENT ON FUNCTION orvex.check_record_holds_released() IS 'Raises OX009 at commit when an executed admission still has an unreleased credit hold (ADR-005).';

CREATE CONSTRAINT TRIGGER chain_records_holds_released AFTER INSERT ON orvex.chain_records
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION orvex.check_record_holds_released();
