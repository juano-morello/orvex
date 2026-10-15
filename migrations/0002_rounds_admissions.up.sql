-- 0002 rounds_admissions: rounds, round secrets, admissions and the admission head (LBT-10 spec P-14, P-15, P-25)

CREATE TABLE orvex.rounds (
    round_id              uuid   NOT NULL,
    game                  text   NOT NULL,
    rules_version         bigint NOT NULL,
    schema_version        bigint NOT NULL,
    state_version         bigint NOT NULL,
    chain                 text   NOT NULL,
    created_at_unix_micro bigint NOT NULL,
    CONSTRAINT rounds_pkey PRIMARY KEY (round_id),
    CONSTRAINT rounds_round_id_check CHECK (round_id <> '00000000-0000-0000-0000-000000000000'::uuid),
    CONSTRAINT rounds_game_check CHECK (game ~ '^[a-z][a-z0-9-]{0,31}$'),
    CONSTRAINT rounds_versions_check CHECK (
        rules_version BETWEEN 1 AND 4294967295
        AND schema_version BETWEEN 1 AND 4294967295
        AND state_version BETWEEN 1 AND 4294967295),
    CONSTRAINT rounds_chain_check CHECK (chain = 'orvex/audit/v1')
);

COMMENT ON TABLE orvex.rounds IS 'Immutable round identity and registry key, derived from the genesis record (ADR-005, ADR-001).';

CREATE TRIGGER rounds_append_only BEFORE UPDATE OR DELETE ON orvex.rounds
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER rounds_no_truncate BEFORE TRUNCATE ON orvex.rounds
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE TABLE orvex.round_secrets (
    round_id      uuid  NOT NULL,
    config_secret bytea NOT NULL,
    CONSTRAINT round_secrets_pkey PRIMARY KEY (round_id),
    CONSTRAINT round_secrets_round_fkey FOREIGN KEY (round_id) REFERENCES orvex.rounds (round_id),
    CONSTRAINT round_secrets_config_secret_check CHECK (octet_length(config_secret) >= 2)
);

COMMENT ON TABLE orvex.round_secrets IS 'Codec secret config bytes (the seed), stored unencrypted outside chain records (ADR-004, ADR-005).';

CREATE TRIGGER round_secrets_append_only BEFORE UPDATE OR DELETE ON orvex.round_secrets
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER round_secrets_no_truncate BEFORE TRUNCATE ON orvex.round_secrets
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE TABLE orvex.admissions (
    round_id                  uuid   NOT NULL,
    sequence                  bigint NOT NULL,
    prev_sequence             bigint GENERATED ALWAYS AS (NULLIF(sequence, 1) - 1) STORED,
    command_id                uuid   NOT NULL,
    effective_time_unix_micro bigint NOT NULL,
    command                   bytea  NOT NULL,
    CONSTRAINT admissions_pkey PRIMARY KEY (round_id, sequence),
    CONSTRAINT admissions_round_fkey FOREIGN KEY (round_id) REFERENCES orvex.rounds (round_id),
    CONSTRAINT admissions_sequence_check CHECK (sequence >= 1),
    CONSTRAINT admissions_prev_fkey FOREIGN KEY (round_id, prev_sequence)
        REFERENCES orvex.admissions (round_id, sequence),
    CONSTRAINT admissions_command_id_key UNIQUE (command_id),
    CONSTRAINT admissions_command_id_check CHECK (command_id <> '00000000-0000-0000-0000-000000000000'::uuid),
    CONSTRAINT admissions_command_check CHECK (octet_length(command) >= 2)
);

COMMENT ON TABLE orvex.admissions IS 'Durably admitted commands with gapless per-round sequence and effective time (ADR-002, ADR-005).';

CREATE TRIGGER admissions_append_only BEFORE UPDATE OR DELETE ON orvex.admissions
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER admissions_no_truncate BEFORE TRUNCATE ON orvex.admissions
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE TABLE orvex.round_admission_heads (
    round_id          uuid   NOT NULL,
    admitted_sequence bigint NOT NULL DEFAULT 0,
    admitted_ref      bigint GENERATED ALWAYS AS (NULLIF(admitted_sequence, 0)) STORED,
    CONSTRAINT round_admission_heads_pkey PRIMARY KEY (round_id),
    CONSTRAINT round_admission_heads_round_fkey FOREIGN KEY (round_id) REFERENCES orvex.rounds (round_id),
    CONSTRAINT round_admission_heads_admitted_sequence_check CHECK (admitted_sequence >= 0),
    CONSTRAINT round_admission_heads_head_fkey FOREIGN KEY (round_id, admitted_ref)
        REFERENCES orvex.admissions (round_id, sequence) DEFERRABLE INITIALLY DEFERRED
);

COMMENT ON TABLE orvex.round_admission_heads IS 'Per-round admission counter and Transaction A lock row (ADR-002, ADR-005).';

CREATE FUNCTION orvex.check_admission_head() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.round_id IS DISTINCT FROM OLD.round_id
        OR NEW.admitted_sequence < OLD.admitted_sequence THEN
        RAISE EXCEPTION 'orvex.round_admission_heads: head of round % moves backwards', OLD.round_id
            USING ERRCODE = 'OX005';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER round_admission_heads_forward BEFORE UPDATE ON orvex.round_admission_heads
    FOR EACH ROW EXECUTE FUNCTION orvex.check_admission_head();
CREATE TRIGGER round_admission_heads_no_delete BEFORE DELETE ON orvex.round_admission_heads
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER round_admission_heads_no_truncate BEFORE TRUNCATE ON orvex.round_admission_heads
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();
