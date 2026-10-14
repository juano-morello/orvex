-- 0002 rounds_admissions: drop the round and admission tables, refusing when any holds rows (LBT-10 spec P-5)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orvex.round_admission_heads) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_admission_heads holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.admissions) THEN
        RAISE EXCEPTION 'down migration refused: orvex.admissions holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.round_secrets) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_secrets holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.rounds) THEN
        RAISE EXCEPTION 'down migration refused: orvex.rounds holds rows' USING ERRCODE = 'OX003';
    END IF;
END
$$;

DROP TABLE orvex.round_admission_heads;
DROP TABLE orvex.admissions;
DROP TABLE orvex.round_secrets;
DROP TABLE orvex.rounds;

DROP FUNCTION orvex.check_admission_head();
