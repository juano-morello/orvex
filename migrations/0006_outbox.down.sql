-- 0006 outbox: drop the outbox, refusing when it holds rows (LBT-10 spec P-5)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orvex.outbox_messages) THEN
        RAISE EXCEPTION 'down migration refused: orvex.outbox_messages holds rows' USING ERRCODE = 'OX003';
    END IF;
END
$$;

DROP TABLE orvex.outbox_messages;

DROP FUNCTION orvex.guard_outbox_message();
