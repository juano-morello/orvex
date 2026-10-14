-- 0004 snapshots: engine-computed state hashes per chain record, the export checkpoints (LBT-10 spec P-20)

CREATE TABLE orvex.round_snapshots (
    round_id     uuid        NOT NULL,
    sequence     bigint      NOT NULL,
    state_hash   bytea       NOT NULL,
    state        bytea,
    engine_build text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT round_snapshots_pkey PRIMARY KEY (round_id, sequence),
    CONSTRAINT round_snapshots_sequence_check CHECK (sequence >= 0),
    CONSTRAINT round_snapshots_state_hash_check CHECK (octet_length(state_hash) = 32),
    CONSTRAINT round_snapshots_engine_build_check CHECK (engine_build ~ '^[!-~]{1,128}$'),
    CONSTRAINT round_snapshots_record_fkey FOREIGN KEY (round_id, sequence)
        REFERENCES orvex.chain_records (round_id, sequence)
);

COMMENT ON TABLE orvex.round_snapshots IS 'Engine-computed state hashes after chain records: audit checkpoints and recovery anchors (ADR-001, ADR-005).';

CREATE TRIGGER round_snapshots_append_only BEFORE UPDATE OR DELETE ON orvex.round_snapshots
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER round_snapshots_no_truncate BEFORE TRUNCATE ON orvex.round_snapshots
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();
