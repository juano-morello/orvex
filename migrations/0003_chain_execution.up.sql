-- 0003 chain_execution: chain records, execution head with fencing, domain events and command results (LBT-10 spec P-16 to P-19)

CREATE TABLE orvex.chain_records (
    round_id           uuid   NOT NULL,
    sequence           bigint NOT NULL,
    prev_sequence      bigint GENERATED ALWAYS AS (NULLIF(sequence, 0) - 1) STORED,
    admission_sequence bigint GENERATED ALWAYS AS (NULLIF(sequence, 0)) STORED,
    prev_hash          bytea  NOT NULL,
    hash               bytea  NOT NULL,
    record             bytea  NOT NULL,
    result_status      text,
    result_reason      text,
    fencing_token      bigint NOT NULL,
    CONSTRAINT chain_records_pkey PRIMARY KEY (round_id, sequence),
    CONSTRAINT chain_records_link_key UNIQUE (round_id, sequence, hash),
    CONSTRAINT chain_records_round_fkey FOREIGN KEY (round_id) REFERENCES orvex.rounds (round_id),
    CONSTRAINT chain_records_prev_fkey FOREIGN KEY (round_id, prev_sequence, prev_hash)
        REFERENCES orvex.chain_records (round_id, sequence, hash),
    CONSTRAINT chain_records_admission_fkey FOREIGN KEY (round_id, admission_sequence)
        REFERENCES orvex.admissions (round_id, sequence),
    CONSTRAINT chain_records_sequence_check CHECK (sequence >= 0),
    CONSTRAINT chain_records_prev_hash_check CHECK (octet_length(prev_hash) = 32),
    CONSTRAINT chain_records_genesis_check CHECK (
        sequence <> 0 OR prev_hash = decode(repeat('00', 32), 'hex')),
    CONSTRAINT chain_records_hash_length_check CHECK (octet_length(hash) = 32),
    CONSTRAINT chain_records_hash_check CHECK (
        hash = sha256('orvex/audit/v1/record:'::bytea || record)),
    CONSTRAINT chain_records_record_check CHECK (
        octet_length(record) <= 67108864 AND position(decode('0a', 'hex') IN record) = 0),
    CONSTRAINT chain_records_result_check CHECK (
        CASE
            WHEN sequence = 0 THEN result_status IS NULL AND result_reason IS NULL
            WHEN result_status = 'accepted' THEN result_reason IS NULL
            WHEN result_status = 'rejected' THEN
                result_reason IS NOT NULL AND result_reason ~ '^[a-z][a-z0-9_]*$'
            ELSE false
        END),
    CONSTRAINT chain_records_fencing_token_check CHECK (fencing_token >= 0)
);

COMMENT ON TABLE orvex.chain_records IS 'Authoritative per-round hash chain: exact canonical record bytes with link hashes (ADR-001, ADR-005).';

-- check_record_binding ties a record's fixed prefix and suffix to its columns,
-- its admission and its round (P-16, section 5.3). Missing or malformed
-- inputs are left to the NOT NULL, CHECK and foreign-key constraints.
CREATE FUNCTION orvex.check_record_binding() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    v_round     orvex.rounds%ROWTYPE;
    v_admission orvex.admissions%ROWTYPE;
    v_result    text;
    v_prefix    bytea;
    v_suffix    bytea;
BEGIN
    IF NEW.round_id IS NULL OR NEW.sequence IS NULL OR NEW.record IS NULL
        OR NEW.prev_hash IS NULL OR NEW.sequence < 0 THEN
        RETURN NEW;
    END IF;
    SELECT * INTO v_round FROM orvex.rounds WHERE round_id = NEW.round_id;
    IF NOT FOUND THEN
        RETURN NEW;
    END IF;
    IF NEW.sequence = 0 THEN
        v_prefix := convert_to('{"chain":"' || v_round.chain || '","config":{', 'UTF8');
        v_suffix := convert_to(
            ',"game":"' || v_round.game
            || '","kind":"genesis","prevHash":"' || repeat('0', 64)
            || '","roundId":"' || NEW.round_id::text
            || '","rulesVersion":"' || v_round.rules_version::text
            || '","schemaVersion":"' || v_round.schema_version::text
            || '","sequence":"0","stateVersion":"' || v_round.state_version::text
            || '"}', 'UTF8');
    ELSE
        SELECT * INTO v_admission FROM orvex.admissions
            WHERE round_id = NEW.round_id AND sequence = NEW.sequence;
        IF NOT FOUND THEN
            RETURN NEW;
        END IF;
        IF NEW.result_status = 'accepted' THEN
            v_result := '{"status":"accepted"}';
        ELSIF NEW.result_status = 'rejected' AND NEW.result_reason IS NOT NULL THEN
            v_result := '{"reason":"' || NEW.result_reason || '","status":"rejected"}';
        ELSE
            RETURN NEW;
        END IF;
        v_prefix := convert_to('{"command":', 'UTF8')
            || v_admission.command
            || convert_to(
                ',"commandId":"' || v_admission.command_id::text
                || '","effectiveTimeUnixMicro":"' || v_admission.effective_time_unix_micro::text
                || '","engineBuild":"', 'UTF8');
        v_suffix := convert_to(
            '],"kind":"command","prevHash":"' || encode(NEW.prev_hash, 'hex')
            || '","result":' || v_result
            || ',"roundId":"' || NEW.round_id::text
            || '","sequence":"' || NEW.sequence::text
            || '"}', 'UTF8');
    END IF;
    IF octet_length(NEW.record) < octet_length(v_prefix) + octet_length(v_suffix)
        OR substring(NEW.record FROM 1 FOR octet_length(v_prefix)) <> v_prefix
        OR substring(NEW.record FROM octet_length(NEW.record) - octet_length(v_suffix) + 1) <> v_suffix THEN
        RAISE EXCEPTION 'orvex.chain_records: record % of round % disagrees with its columns, admission or round',
            NEW.sequence, NEW.round_id
            USING ERRCODE = 'OX008';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER chain_records_binding BEFORE INSERT ON orvex.chain_records
    FOR EACH ROW EXECUTE FUNCTION orvex.check_record_binding();
CREATE TRIGGER chain_records_append_only BEFORE UPDATE OR DELETE ON orvex.chain_records
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER chain_records_no_truncate BEFORE TRUNCATE ON orvex.chain_records
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE TABLE orvex.round_execution_heads (
    round_id           uuid   NOT NULL,
    processed_sequence bigint NOT NULL DEFAULT 0,
    fencing_token      bigint NOT NULL DEFAULT 0,
    owner_id           text,
    lease_expires_at   timestamptz,
    CONSTRAINT round_execution_heads_pkey PRIMARY KEY (round_id),
    CONSTRAINT round_execution_heads_round_fkey FOREIGN KEY (round_id) REFERENCES orvex.rounds (round_id),
    CONSTRAINT round_execution_heads_processed_sequence_check CHECK (processed_sequence >= 0),
    CONSTRAINT round_execution_heads_fencing_token_check CHECK (fencing_token >= 0),
    CONSTRAINT round_execution_heads_owner_check CHECK (
        (owner_id IS NULL) = (lease_expires_at IS NULL)
        AND (owner_id IS NULL OR owner_id ~ '^[!-~]{1,128}$')),
    CONSTRAINT round_execution_heads_head_fkey FOREIGN KEY (round_id, processed_sequence)
        REFERENCES orvex.chain_records (round_id, sequence) DEFERRABLE INITIALLY DEFERRED
);

COMMENT ON TABLE orvex.round_execution_heads IS 'Per-round processed sequence, ownership and fencing token; Transaction B compare-and-set row (ADR-005).';

CREATE FUNCTION orvex.check_execution_head() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.round_id IS DISTINCT FROM OLD.round_id
        OR NEW.processed_sequence < OLD.processed_sequence
        OR NEW.fencing_token < OLD.fencing_token
        OR (NEW.owner_id IS DISTINCT FROM OLD.owner_id AND NEW.fencing_token <= OLD.fencing_token) THEN
        RAISE EXCEPTION 'orvex.round_execution_heads: head of round % moves backwards or changes owner without a new fencing token',
            OLD.round_id
            USING ERRCODE = 'OX005';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER round_execution_heads_forward BEFORE UPDATE ON orvex.round_execution_heads
    FOR EACH ROW EXECUTE FUNCTION orvex.check_execution_head();
CREATE TRIGGER round_execution_heads_no_delete BEFORE DELETE ON orvex.round_execution_heads
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER round_execution_heads_no_truncate BEFORE TRUNCATE ON orvex.round_execution_heads
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

-- check_record_fenced runs at commit for every inserted record (P-19): the
-- record must have been written under the round's current fencing token and
-- covered by its processed sequence.
CREATE FUNCTION orvex.check_record_fenced() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    v_token     bigint;
    v_processed bigint;
BEGIN
    SELECT fencing_token, processed_sequence INTO v_token, v_processed
        FROM orvex.round_execution_heads
        WHERE round_id = NEW.round_id
        FOR SHARE;
    IF NOT FOUND OR v_token <> NEW.fencing_token OR v_processed < NEW.sequence THEN
        RAISE EXCEPTION 'orvex.chain_records: record % of round % is not fenced by the current execution head',
            NEW.sequence, NEW.round_id
            USING ERRCODE = 'OX004';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER chain_records_fenced AFTER INSERT ON orvex.chain_records
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION orvex.check_record_fenced();

CREATE TABLE orvex.domain_events (
    round_id            uuid   NOT NULL,
    event_sequence      bigint NOT NULL,
    prev_event_sequence bigint GENERATED ALWAYS AS (NULLIF(event_sequence, 1) - 1) STORED,
    sequence            bigint NOT NULL,
    event_type          text   NOT NULL,
    CONSTRAINT domain_events_pkey PRIMARY KEY (round_id, event_sequence),
    CONSTRAINT domain_events_event_sequence_check CHECK (event_sequence >= 1),
    CONSTRAINT domain_events_sequence_check CHECK (sequence >= 1),
    CONSTRAINT domain_events_event_type_check CHECK (event_type ~ '^[A-Z][A-Za-z0-9]{0,63}$'),
    CONSTRAINT domain_events_prev_fkey FOREIGN KEY (round_id, prev_event_sequence)
        REFERENCES orvex.domain_events (round_id, event_sequence),
    CONSTRAINT domain_events_record_fkey FOREIGN KEY (round_id, sequence)
        REFERENCES orvex.chain_records (round_id, sequence)
);

CREATE INDEX domain_events_record_idx ON orvex.domain_events (round_id, sequence);

COMMENT ON TABLE orvex.domain_events IS 'Derived index of chained domain events without payload copies (ADR-005, ADR-001).';

CREATE TRIGGER domain_events_append_only BEFORE UPDATE OR DELETE ON orvex.domain_events
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER domain_events_no_truncate BEFORE TRUNCATE ON orvex.domain_events
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE VIEW orvex.command_results AS
    SELECT a.command_id,
           round_id,
           sequence,
           a.effective_time_unix_micro,
           r.result_status AS status,
           r.result_reason AS reason
    FROM orvex.admissions a
    JOIN orvex.chain_records r USING (round_id, sequence);

COMMENT ON VIEW orvex.command_results IS 'Terminal command results of executed admissions, bound to chain record bytes (ADR-005).';
