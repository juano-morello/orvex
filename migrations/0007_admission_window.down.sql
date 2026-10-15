-- 0007 admission_window: drop the admission window and admit_command, refusing when round_admission_heads holds rows (LBT-11 spec S-8, §5.4)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orvex.round_admission_heads) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_admission_heads holds rows' USING ERRCODE = 'OX003';
    END IF;
END
$$;

DROP FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, bigint, integer, integer);

DROP TRIGGER round_admission_heads_window ON orvex.round_admission_heads;
DROP FUNCTION orvex.check_admission_window();

ALTER TABLE orvex.round_admission_heads
    DROP CONSTRAINT round_admission_heads_sealed_fkey,
    DROP CONSTRAINT round_admission_heads_closed_fkey,
    DROP CONSTRAINT round_admission_heads_started_fkey,
    DROP CONSTRAINT round_admission_heads_window_check,
    DROP CONSTRAINT round_admission_heads_state_check,
    DROP COLUMN sealed_sequence,
    DROP COLUMN closed_sequence,
    DROP COLUMN started_sequence,
    DROP COLUMN admission_state;

DROP FUNCTION orvex.unix_micro(timestamptz);
