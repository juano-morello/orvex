-- 0005 ledger: drop the ledger tables and functions, refusing when any table holds rows (LBT-10 spec P-5)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orvex.ledger_hold_releases) THEN
        RAISE EXCEPTION 'down migration refused: orvex.ledger_hold_releases holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.ledger_holds) THEN
        RAISE EXCEPTION 'down migration refused: orvex.ledger_holds holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.ledger_postings) THEN
        RAISE EXCEPTION 'down migration refused: orvex.ledger_postings holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.ledger_transactions) THEN
        RAISE EXCEPTION 'down migration refused: orvex.ledger_transactions holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.ledger_accounts) THEN
        RAISE EXCEPTION 'down migration refused: orvex.ledger_accounts holds rows' USING ERRCODE = 'OX003';
    END IF;
END
$$;

DROP TABLE orvex.ledger_hold_releases;
DROP TABLE orvex.ledger_holds;
DROP TABLE orvex.ledger_postings;
DROP TABLE orvex.ledger_transactions;
DROP TABLE orvex.ledger_accounts;

DROP FUNCTION orvex.apply_ledger_hold_release();
DROP FUNCTION orvex.apply_ledger_hold();
DROP FUNCTION orvex.apply_ledger_posting();
DROP FUNCTION orvex.guard_ledger_account();
