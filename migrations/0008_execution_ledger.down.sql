-- 0008 execution_ledger: drop the hold-release check and restore the non-unique event index (LBT-12 spec E-10, E-11)

DROP TRIGGER chain_records_holds_released ON orvex.chain_records;
DROP FUNCTION orvex.check_record_holds_released();

DROP INDEX orvex.ledger_postings_event_key;

CREATE INDEX ledger_postings_event_idx ON orvex.ledger_postings (round_id, event_sequence)
    WHERE event_sequence IS NOT NULL;
