-- 0006 outbox: transactional outbox with a stable dedupe key and mutable delivery columns (LBT-10 spec P-22)

CREATE TABLE orvex.outbox_messages (
    message_id      bigint      GENERATED ALWAYS AS IDENTITY,
    round_id        uuid        NOT NULL,
    sequence        bigint      NOT NULL,
    message_index   integer     NOT NULL,
    topic           text        NOT NULL,
    payload         bytea       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,
    published_at    timestamptz,
    CONSTRAINT outbox_messages_pkey PRIMARY KEY (message_id),
    CONSTRAINT outbox_messages_record_fkey FOREIGN KEY (round_id, sequence)
        REFERENCES orvex.chain_records (round_id, sequence),
    CONSTRAINT outbox_messages_message_index_check CHECK (message_index >= 0),
    CONSTRAINT outbox_messages_dedupe_key UNIQUE (round_id, sequence, message_index),
    CONSTRAINT outbox_messages_topic_check CHECK (topic ~ '^[a-z][a-z0-9_.-]{0,127}$'),
    CONSTRAINT outbox_messages_attempts_check CHECK (attempts >= 0)
);

CREATE INDEX outbox_messages_pending_idx ON orvex.outbox_messages (next_attempt_at, message_id)
    WHERE published_at IS NULL;

COMMENT ON TABLE orvex.outbox_messages IS 'Transactional outbox for at-least-once delivery; consumers deduplicate on (round_id, sequence, message_index) (ADR-005).';

-- guard_outbox_message lets an update change only the delivery columns,
-- never lower attempts and never touch a published row; only published rows
-- may be deleted (P-22).
CREATE FUNCTION orvex.guard_outbox_message() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.published_at IS NULL THEN
            RAISE EXCEPTION 'orvex.outbox_messages: message % is unpublished and cannot be deleted', OLD.message_id
                USING ERRCODE = 'OX006';
        END IF;
        RETURN OLD;
    END IF;
    IF (OLD.published_at IS NOT NULL AND NEW IS DISTINCT FROM OLD)
        OR NEW.message_id IS DISTINCT FROM OLD.message_id
        OR NEW.round_id IS DISTINCT FROM OLD.round_id
        OR NEW.sequence IS DISTINCT FROM OLD.sequence
        OR NEW.message_index IS DISTINCT FROM OLD.message_index
        OR NEW.topic IS DISTINCT FROM OLD.topic
        OR NEW.payload IS DISTINCT FROM OLD.payload
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.attempts < OLD.attempts THEN
        RAISE EXCEPTION 'orvex.outbox_messages: message % may change only its delivery columns', OLD.message_id
            USING ERRCODE = 'OX006';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER outbox_messages_guard BEFORE UPDATE OR DELETE ON orvex.outbox_messages
    FOR EACH ROW EXECUTE FUNCTION orvex.guard_outbox_message();
CREATE TRIGGER outbox_messages_no_truncate BEFORE TRUNCATE ON orvex.outbox_messages
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();
