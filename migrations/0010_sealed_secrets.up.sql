-- 0010 sealed_secrets: sealed round secrets, revealed secrets, legacy reveal backfill (LBT-21 spec K-5, K-6, K-11)

-- 1. round_secrets: the sealed form (K-5). The engine seals in Go; no key
-- ever reaches the database. The shape and form checks are validated
-- against existing rows, which are legacy plaintext and pass. Every branch
-- states IS NOT NULL explicitly: a CHECK whose expression is NULL passes.
ALTER TABLE orvex.round_secrets
    ALTER COLUMN config_secret DROP NOT NULL,
    ADD COLUMN secret_scheme     text,
    ADD COLUMN secret_key_id     text,
    ADD COLUMN secret_nonce      bytea,
    ADD COLUMN secret_ciphertext bytea,
    ADD CONSTRAINT round_secrets_sealed_shape_check CHECK (
        (secret_scheme IS NULL AND secret_key_id IS NULL AND secret_nonce IS NULL AND secret_ciphertext IS NULL)
        OR (secret_scheme IS NOT NULL AND secret_key_id IS NOT NULL
            AND secret_nonce IS NOT NULL AND secret_ciphertext IS NOT NULL
            AND secret_scheme = 'aes-256-gcm/v1'
            AND secret_key_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$'
            AND octet_length(secret_nonce) = 12
            AND octet_length(secret_ciphertext) >= 18)),
    ADD CONSTRAINT round_secrets_form_check CHECK ((config_secret IS NULL) <> (secret_ciphertext IS NULL));

-- NOT VALID: enforced on every new row, never checked on rows written before
-- 0010 (legacy plaintext). Never VALIDATE it while legacy rows exist (R-2).
ALTER TABLE orvex.round_secrets
    ADD CONSTRAINT round_secrets_sealed_check CHECK (config_secret IS NULL) NOT VALID;

COMMENT ON TABLE orvex.round_secrets IS 'Codec secret config of each round, outside chain records: from 0010 stored only sealed with AES-256-GCM in the secret_* columns; config_secret holds plaintext only in rows written before 0010; close_offset_micros is the secret close offset (ADR-004, ADR-005; LBT-21).';
COMMENT ON COLUMN orvex.round_secrets.config_secret IS 'Plaintext codec secret config, only in legacy rows written before 0010; NULL in every sealed row (LBT-21 K-5).';
COMMENT ON COLUMN orvex.round_secrets.secret_scheme IS 'Sealing scheme of the sealed secret config: aes-256-gcm/v1 (LBT-21 K-5).';
COMMENT ON COLUMN orvex.round_secrets.secret_key_id IS 'ID of the keyring key that sealed the secret config; the key itself never reaches the database (LBT-21 K-4, K-5).';
COMMENT ON COLUMN orvex.round_secrets.secret_nonce IS 'Random 96-bit GCM nonce of the sealed secret config (LBT-21 K-5).';
COMMENT ON COLUMN orvex.round_secrets.secret_ciphertext IS 'AES-256-GCM ciphertext of the secret config followed by the 16-byte tag; the AAD binds scheme, key ID and round ID (LBT-21 K-5).';

-- 2. round_secret_reveals: the secret config revealed at settlement (K-6).
-- check_secret_reveal leaves NULL inputs and sequence < 1 to the constraints
-- and a missing record to the foreign key, as check_record_binding does.
CREATE FUNCTION orvex.check_secret_reveal() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    v_status text;
BEGIN
    IF NEW.round_id IS NULL OR NEW.sequence IS NULL OR NEW.sequence < 1 THEN
        RETURN NEW;
    END IF;
    SELECT c.result_status INTO v_status
        FROM orvex.chain_records c
        WHERE c.round_id = NEW.round_id AND c.sequence = NEW.sequence;
    IF NOT FOUND THEN
        RETURN NEW;
    END IF;
    IF v_status IS DISTINCT FROM 'accepted' THEN
        RAISE EXCEPTION 'orvex.round_secret_reveals: record % of round % is not an accepted command record',
            NEW.sequence, NEW.round_id
            USING ERRCODE = 'OX010';
    END IF;
    RETURN NEW;
END
$$;

COMMENT ON FUNCTION orvex.check_secret_reveal() IS 'Raises OX010 when a reveal names a chain record that is not an accepted command record of its round (ADR-004, ADR-005).';

CREATE TABLE orvex.round_secret_reveals (
    round_id      uuid   NOT NULL,
    sequence      bigint NOT NULL,
    config_secret bytea  NOT NULL,
    CONSTRAINT round_secret_reveals_pkey PRIMARY KEY (round_id),
    CONSTRAINT round_secret_reveals_secret_fkey FOREIGN KEY (round_id) REFERENCES orvex.round_secrets (round_id),
    CONSTRAINT round_secret_reveals_record_fkey FOREIGN KEY (round_id, sequence)
        REFERENCES orvex.chain_records (round_id, sequence),
    CONSTRAINT round_secret_reveals_sequence_check CHECK (sequence >= 1),
    CONSTRAINT round_secret_reveals_config_secret_check CHECK (octet_length(config_secret) >= 2)
);

COMMENT ON TABLE orvex.round_secret_reveals IS 'Secret config revealed at settlement: written in the Transaction B of the record that finalizes the round; public from then on (ADR-004, ADR-005).';

CREATE TRIGGER round_secret_reveals_record BEFORE INSERT ON orvex.round_secret_reveals
    FOR EACH ROW EXECUTE FUNCTION orvex.check_secret_reveal();
CREATE TRIGGER round_secret_reveals_append_only BEFORE UPDATE OR DELETE ON orvex.round_secret_reveals
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER round_secret_reveals_no_truncate BEFORE TRUNCATE ON orvex.round_secret_reveals
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

-- 3. Backfill reveals for legacy settled rounds (K-11): a plaintext row
-- whose admission window is sealed at an accepted record. The predicate is
-- game-agnostic: the rules policy assigns the window classes at admission.
INSERT INTO orvex.round_secret_reveals (round_id, sequence, config_secret)
SELECT s.round_id, h.sealed_sequence, s.config_secret
    FROM orvex.round_secrets s
    JOIN orvex.round_admission_heads h ON h.round_id = s.round_id AND h.admission_state = 'sealed'
    JOIN orvex.chain_records c ON c.round_id = s.round_id AND c.sequence = h.sealed_sequence
        AND c.result_status = 'accepted'
    WHERE s.config_secret IS NOT NULL
    ORDER BY s.round_id;
