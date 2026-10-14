-- 0005 ledger: accounts with trigger-maintained totals, transactions, two-sided postings, holds and releases (LBT-10 spec P-21)

CREATE TABLE orvex.ledger_accounts (
    account_id     uuid        NOT NULL,
    kind           text        NOT NULL,
    player_id      text,
    name           text,
    round_id       uuid,
    floor_enforced boolean     NOT NULL,
    debits_posted  bigint      NOT NULL DEFAULT 0,
    credits_posted bigint      NOT NULL DEFAULT 0,
    held           bigint      NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_accounts_pkey PRIMARY KEY (account_id),
    CONSTRAINT ledger_accounts_kind_check CHECK (
        (kind = 'player' AND player_id IS NOT NULL AND player_id <> '' AND name IS NULL
            AND round_id IS NULL AND floor_enforced)
        OR (kind = 'system' AND player_id IS NULL AND name IS NOT NULL)),
    CONSTRAINT ledger_accounts_player_id_key UNIQUE (player_id),
    CONSTRAINT ledger_accounts_name_check CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'),
    CONSTRAINT ledger_accounts_round_fkey FOREIGN KEY (round_id) REFERENCES orvex.rounds (round_id),
    CONSTRAINT ledger_accounts_totals_check CHECK (
        debits_posted >= 0 AND credits_posted >= 0 AND held >= 0),
    CONSTRAINT ledger_accounts_floor_check CHECK (
        NOT floor_enforced OR debits_posted + held <= credits_posted)
);

-- A partial index: a table-level UNIQUE NULLS NOT DISTINCT (round_id, name)
-- would also make two player accounts collide on (NULL, NULL).
CREATE UNIQUE INDEX ledger_accounts_system_name_key ON orvex.ledger_accounts (round_id, name)
    NULLS NOT DISTINCT WHERE kind = 'system';

COMMENT ON TABLE orvex.ledger_accounts IS 'Ledger accounts with totals maintained only by ledger triggers and a floor check (ADR-005).';

CREATE FUNCTION orvex.guard_ledger_account() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.debits_posted IS DISTINCT FROM 0 OR NEW.credits_posted IS DISTINCT FROM 0
            OR NEW.held IS DISTINCT FROM 0 THEN
            RAISE EXCEPTION 'orvex.ledger_accounts: account % must start with zero totals', NEW.account_id
                USING ERRCODE = 'OX002';
        END IF;
        RETURN NEW;
    END IF;
    IF pg_trigger_depth() < 2
        OR NEW.account_id IS DISTINCT FROM OLD.account_id
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.player_id IS DISTINCT FROM OLD.player_id
        OR NEW.name IS DISTINCT FROM OLD.name
        OR NEW.round_id IS DISTINCT FROM OLD.round_id
        OR NEW.floor_enforced IS DISTINCT FROM OLD.floor_enforced
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'orvex.ledger_accounts: account % changes only through ledger postings and holds', OLD.account_id
            USING ERRCODE = 'OX002';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER ledger_accounts_guard BEFORE INSERT OR UPDATE ON orvex.ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION orvex.guard_ledger_account();
CREATE TRIGGER ledger_accounts_no_delete BEFORE DELETE ON orvex.ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER ledger_accounts_no_truncate BEFORE TRUNCATE ON orvex.ledger_accounts
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE TABLE orvex.ledger_transactions (
    transaction_id uuid        NOT NULL,
    kind           text        NOT NULL,
    round_id       uuid,
    sequence       bigint,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_transactions_pkey PRIMARY KEY (transaction_id),
    CONSTRAINT ledger_transactions_kind_check CHECK (kind ~ '^[a-z][a-z0-9_]{0,63}$'),
    CONSTRAINT ledger_transactions_record_check CHECK ((round_id IS NULL) = (sequence IS NULL)),
    CONSTRAINT ledger_transactions_record_fkey FOREIGN KEY (round_id, sequence)
        REFERENCES orvex.chain_records (round_id, sequence),
    CONSTRAINT ledger_transactions_record_key UNIQUE (round_id, sequence),
    CONSTRAINT ledger_transactions_round_key UNIQUE (transaction_id, round_id)
);

COMMENT ON TABLE orvex.ledger_transactions IS 'One ledger transaction per business operation, at most one per chain record (ADR-005).';

CREATE TRIGGER ledger_transactions_append_only BEFORE UPDATE OR DELETE ON orvex.ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER ledger_transactions_no_truncate BEFORE TRUNCATE ON orvex.ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE TABLE orvex.ledger_postings (
    transaction_id    uuid    NOT NULL,
    posting_index     integer NOT NULL,
    debit_account_id  uuid    NOT NULL,
    credit_account_id uuid    NOT NULL,
    amount            bigint  NOT NULL,
    round_id          uuid,
    event_sequence    bigint,
    CONSTRAINT ledger_postings_pkey PRIMARY KEY (transaction_id, posting_index),
    CONSTRAINT ledger_postings_transaction_fkey FOREIGN KEY (transaction_id)
        REFERENCES orvex.ledger_transactions (transaction_id),
    CONSTRAINT ledger_postings_posting_index_check CHECK (posting_index >= 0),
    CONSTRAINT ledger_postings_debit_account_fkey FOREIGN KEY (debit_account_id)
        REFERENCES orvex.ledger_accounts (account_id),
    CONSTRAINT ledger_postings_credit_account_fkey FOREIGN KEY (credit_account_id)
        REFERENCES orvex.ledger_accounts (account_id),
    CONSTRAINT ledger_postings_accounts_check CHECK (debit_account_id <> credit_account_id),
    CONSTRAINT ledger_postings_amount_check CHECK (amount > 0),
    CONSTRAINT ledger_postings_round_fkey FOREIGN KEY (transaction_id, round_id)
        REFERENCES orvex.ledger_transactions (transaction_id, round_id),
    CONSTRAINT ledger_postings_event_check CHECK (event_sequence IS NULL OR round_id IS NOT NULL),
    CONSTRAINT ledger_postings_event_fkey FOREIGN KEY (round_id, event_sequence)
        REFERENCES orvex.domain_events (round_id, event_sequence)
);

CREATE INDEX ledger_postings_debit_idx ON orvex.ledger_postings (debit_account_id);
CREATE INDEX ledger_postings_credit_idx ON orvex.ledger_postings (credit_account_id);
CREATE INDEX ledger_postings_event_idx ON orvex.ledger_postings (round_id, event_sequence)
    WHERE event_sequence IS NOT NULL;

COMMENT ON TABLE orvex.ledger_postings IS 'Two-sided postings moving one amount between two accounts, balanced by construction (ADR-005).';

-- apply_ledger_posting adds the amount to both accounts, lower account_id
-- first (section 6 lock order). Totals are updated relatively, so a blocked
-- update re-evaluates the floor check on the newest row version.
CREATE FUNCTION orvex.apply_ledger_posting() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.debit_account_id < NEW.credit_account_id THEN
        UPDATE orvex.ledger_accounts SET debits_posted = debits_posted + NEW.amount
            WHERE account_id = NEW.debit_account_id;
        UPDATE orvex.ledger_accounts SET credits_posted = credits_posted + NEW.amount
            WHERE account_id = NEW.credit_account_id;
    ELSE
        UPDATE orvex.ledger_accounts SET credits_posted = credits_posted + NEW.amount
            WHERE account_id = NEW.credit_account_id;
        UPDATE orvex.ledger_accounts SET debits_posted = debits_posted + NEW.amount
            WHERE account_id = NEW.debit_account_id;
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER ledger_postings_apply AFTER INSERT ON orvex.ledger_postings
    FOR EACH ROW EXECUTE FUNCTION orvex.apply_ledger_posting();
CREATE TRIGGER ledger_postings_append_only BEFORE UPDATE OR DELETE ON orvex.ledger_postings
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER ledger_postings_no_truncate BEFORE TRUNCATE ON orvex.ledger_postings
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE TABLE orvex.ledger_holds (
    round_id   uuid        NOT NULL,
    sequence   bigint      NOT NULL,
    account_id uuid        NOT NULL,
    amount     bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_holds_pkey PRIMARY KEY (round_id, sequence, account_id),
    CONSTRAINT ledger_holds_admission_fkey FOREIGN KEY (round_id, sequence)
        REFERENCES orvex.admissions (round_id, sequence),
    CONSTRAINT ledger_holds_account_fkey FOREIGN KEY (account_id)
        REFERENCES orvex.ledger_accounts (account_id),
    CONSTRAINT ledger_holds_amount_check CHECK (amount > 0)
);

CREATE INDEX ledger_holds_account_idx ON orvex.ledger_holds (account_id);

COMMENT ON TABLE orvex.ledger_holds IS 'Credit reservations made at admission for cross-round balance protection (ADR-005).';

CREATE FUNCTION orvex.apply_ledger_hold() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    UPDATE orvex.ledger_accounts SET held = held + NEW.amount
        WHERE account_id = NEW.account_id;
    RETURN NULL;
END
$$;

CREATE TRIGGER ledger_holds_apply AFTER INSERT ON orvex.ledger_holds
    FOR EACH ROW EXECUTE FUNCTION orvex.apply_ledger_hold();
CREATE TRIGGER ledger_holds_append_only BEFORE UPDATE OR DELETE ON orvex.ledger_holds
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER ledger_holds_no_truncate BEFORE TRUNCATE ON orvex.ledger_holds
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();

CREATE TABLE orvex.ledger_hold_releases (
    round_id   uuid        NOT NULL,
    sequence   bigint      NOT NULL,
    account_id uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_hold_releases_pkey PRIMARY KEY (round_id, sequence, account_id),
    CONSTRAINT ledger_hold_releases_hold_fkey FOREIGN KEY (round_id, sequence, account_id)
        REFERENCES orvex.ledger_holds (round_id, sequence, account_id),
    CONSTRAINT ledger_hold_releases_record_fkey FOREIGN KEY (round_id, sequence)
        REFERENCES orvex.chain_records (round_id, sequence)
);

COMMENT ON TABLE orvex.ledger_hold_releases IS 'Releases of credit holds once the admission''s chain record exists (ADR-005).';

CREATE FUNCTION orvex.apply_ledger_hold_release() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    v_amount bigint;
BEGIN
    SELECT h.amount INTO v_amount
        FROM orvex.ledger_holds h
        WHERE h.round_id = NEW.round_id AND h.sequence = NEW.sequence AND h.account_id = NEW.account_id;
    IF NOT FOUND THEN
        -- ledger_hold_releases_hold_fkey reports the missing hold.
        RETURN NULL;
    END IF;
    UPDATE orvex.ledger_accounts SET held = held - v_amount
        WHERE account_id = NEW.account_id;
    RETURN NULL;
END
$$;

CREATE TRIGGER ledger_hold_releases_apply AFTER INSERT ON orvex.ledger_hold_releases
    FOR EACH ROW EXECUTE FUNCTION orvex.apply_ledger_hold_release();
CREATE TRIGGER ledger_hold_releases_append_only BEFORE UPDATE OR DELETE ON orvex.ledger_hold_releases
    FOR EACH ROW EXECUTE FUNCTION orvex.forbid_mutation();
CREATE TRIGGER ledger_hold_releases_no_truncate BEFORE TRUNCATE ON orvex.ledger_hold_releases
    FOR EACH STATEMENT EXECUTE FUNCTION orvex.forbid_mutation();
