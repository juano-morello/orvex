-- 0010 sealed_secrets: drop sealed and revealed secrets, refusing when round_secret_reveals or sealed round_secrets rows exist (LBT-21 spec K-11)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orvex.round_secret_reveals) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_secret_reveals holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.round_secrets WHERE secret_ciphertext IS NOT NULL) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_secrets holds sealed rows' USING ERRCODE = 'OX003';
    END IF;
END
$$;

DROP TABLE orvex.round_secret_reveals;
DROP FUNCTION orvex.check_secret_reveal();

ALTER TABLE orvex.round_secrets
    DROP CONSTRAINT round_secrets_sealed_check,
    DROP CONSTRAINT round_secrets_form_check,
    DROP CONSTRAINT round_secrets_sealed_shape_check,
    DROP COLUMN secret_ciphertext,
    DROP COLUMN secret_nonce,
    DROP COLUMN secret_key_id,
    DROP COLUMN secret_scheme,
    ALTER COLUMN config_secret SET NOT NULL;

COMMENT ON COLUMN orvex.round_secrets.config_secret IS NULL;
COMMENT ON TABLE orvex.round_secrets IS 'Codec secret config bytes (the seed), stored unencrypted outside chain records (ADR-004, ADR-005).';
