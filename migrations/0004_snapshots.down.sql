-- 0004 snapshots: drop round snapshots, refusing when it holds rows (LBT-10 spec P-5)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orvex.round_snapshots) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_snapshots holds rows' USING ERRCODE = 'OX003';
    END IF;
END
$$;

DROP TABLE orvex.round_snapshots;
