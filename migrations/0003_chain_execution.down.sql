-- 0003 chain_execution: drop chain records, execution heads, domain events and the results view, refusing on rows (LBT-10 spec P-5)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orvex.domain_events) THEN
        RAISE EXCEPTION 'down migration refused: orvex.domain_events holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.round_execution_heads) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_execution_heads holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.chain_records) THEN
        RAISE EXCEPTION 'down migration refused: orvex.chain_records holds rows' USING ERRCODE = 'OX003';
    END IF;
END
$$;

DROP VIEW orvex.command_results;
DROP TABLE orvex.domain_events;
DROP TABLE orvex.round_execution_heads;
DROP TABLE orvex.chain_records;

DROP FUNCTION orvex.check_record_fenced();
DROP FUNCTION orvex.check_execution_head();
DROP FUNCTION orvex.check_record_binding();
