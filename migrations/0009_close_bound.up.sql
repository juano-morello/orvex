-- 0009 close_bound: secret close offset, close bound recorded at start, SECURITY DEFINER admit_command (LBT-12 spec E-16 and engineer addendum; LBT-11 spec S-8)

ALTER TABLE orvex.round_secrets
    ADD COLUMN close_offset_micros bigint,
    ADD CONSTRAINT round_secrets_close_offset_check CHECK (
        close_offset_micros IS NULL OR close_offset_micros >= 0);

COMMENT ON COLUMN orvex.round_secrets.close_offset_micros IS 'Secret offset from the start admission''s effective time before which no closing command is admitted (crash/v1: the crash deadline offset D); written at round creation, read only by orvex.admit_command (ADR-002, ADR-004).';

ALTER TABLE orvex.round_admission_heads
    ADD COLUMN close_not_before_unix_micro bigint,
    ADD CONSTRAINT round_admission_heads_close_bound_check CHECK (
        close_not_before_unix_micro IS NULL OR started_sequence IS NOT NULL);

COMMENT ON COLUMN orvex.round_admission_heads.close_not_before_unix_micro IS 'Secret closure bound T0 + D, set once when the start command is admitted; a closing command with an earlier effective time is not due (ADR-002, ADR-004).';

-- check_close_bound keeps the closure bound immutable once set. The message
-- never formats the bound: it is secret until the crash.
CREATE FUNCTION orvex.check_close_bound() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF OLD.close_not_before_unix_micro IS NOT NULL
        AND NEW.close_not_before_unix_micro IS DISTINCT FROM OLD.close_not_before_unix_micro THEN
        RAISE EXCEPTION 'orvex.round_admission_heads: close bound of round % changes', OLD.round_id
            USING ERRCODE = 'OX005';
    END IF;
    RETURN NEW;
END
$$;

COMMENT ON FUNCTION orvex.check_close_bound() IS 'Raises OX005 when a set closure bound changes (ADR-002).';

CREATE TRIGGER round_admission_heads_close_bound BEFORE UPDATE ON orvex.round_admission_heads
    FOR EACH ROW EXECUTE FUNCTION orvex.check_close_bound();

DROP FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, bigint, integer, integer);

-- admit_command is Transaction A's single statement (S-3, §5.3). Steps 1
-- and 2 are the lock-free fast path (S-5); steps 3 to 13 run under the
-- round's head lock, which the caller's COMMIT or ROLLBACK releases. Every
-- column reference is qualified: the RETURNS TABLE columns sequence and
-- effective_time_unix_micro are also PL/pgSQL variables. The function never
-- catches exceptions. It is SECURITY DEFINER (LBT-12 engineer addendum): it
-- reads the secret close offset and writes the secret closure bound, which
-- the admitting role cannot read. Every relation is schema-qualified and
-- search_path is pg_catalog.
CREATE FUNCTION orvex.admit_command(
    p_round_id              uuid,
    p_game                  text,
    p_rules_version         bigint,
    p_command_id            uuid,
    p_command               bytea,
    p_class                 text,
    p_hold_player_id        text,
    p_hold_credits          bigint,
    p_lock_timeout_ms       integer,
    p_idle_timeout_ms       integer)
    RETURNS TABLE (outcome text, reason text, sequence bigint, effective_time_unix_micro bigint)
    LANGUAGE plpgsql VOLATILE
    SECURITY DEFINER
    SET search_path = pg_catalog
AS $$
DECLARE
    v_found_round   uuid;
    v_found_seq     bigint;
    v_found_time    bigint;
    v_found_command bytea;
    v_game          text;
    v_rules         bigint;
    v_has_head      boolean;
    v_state         text;
    v_admitted      bigint;
    v_clock         bigint;
    v_t             bigint;
    v_prev_t        bigint;
    v_offset        bigint;
    v_bound         bigint;
    v_account       uuid;
    v_available     bigint;
    v_seq           bigint;
BEGIN
    -- Argument checks: caller bugs.
    IF p_round_id IS NULL OR p_game IS NULL OR p_rules_version IS NULL
        OR p_command_id IS NULL OR p_command IS NULL THEN
        RAISE EXCEPTION 'orvex.admit_command: missing argument' USING ERRCODE = '22023';
    END IF;
    IF p_class IS NULL OR p_class NOT IN ('ordinary', 'start', 'close', 'settle') THEN
        RAISE EXCEPTION 'orvex.admit_command: invalid class' USING ERRCODE = '22023';
    END IF;
    IF (p_hold_player_id IS NULL) <> (p_hold_credits IS NULL)
        OR (p_hold_credits IS NOT NULL AND p_hold_credits <= 0) THEN
        RAISE EXCEPTION 'orvex.admit_command: inconsistent hold' USING ERRCODE = '22023';
    END IF;
    IF p_lock_timeout_ms IS NULL OR p_lock_timeout_ms < 1
        OR p_idle_timeout_ms IS NULL OR p_idle_timeout_ms < 1 THEN
        RAISE EXCEPTION 'orvex.admit_command: invalid timeout' USING ERRCODE = '22023';
    END IF;

    -- 0. Transaction-local settings.
    PERFORM set_config('lock_timeout', p_lock_timeout_ms::text || 'ms', true);
    PERFORM set_config('idle_in_transaction_session_timeout', p_idle_timeout_ms::text || 'ms', true);
    IF current_setting('synchronous_commit') = 'off' THEN
        PERFORM set_config('synchronous_commit', 'on', true);
    END IF;

    -- 1. Fast dedupe, no lock.
    SELECT a.round_id, a.sequence, a.effective_time_unix_micro, a.command
        INTO v_found_round, v_found_seq, v_found_time, v_found_command
        FROM orvex.admissions a
        WHERE a.command_id = p_command_id;
    IF FOUND THEN
        IF v_found_round = p_round_id AND v_found_command = p_command THEN
            RETURN QUERY SELECT 'duplicate'::text, NULL::text, v_found_seq, v_found_time;
        ELSE
            RETURN QUERY SELECT 'conflict'::text, NULL::text, NULL::bigint, NULL::bigint;
        END IF;
        RETURN;
    END IF;

    -- 2. Fast window, no lock.
    SELECT r.game, r.rules_version, h.round_id IS NOT NULL, h.admission_state
        INTO v_game, v_rules, v_has_head, v_state
        FROM orvex.rounds r
        LEFT JOIN orvex.round_admission_heads h ON h.round_id = r.round_id
        WHERE r.round_id = p_round_id;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'not_found'::text, NULL::text, NULL::bigint, NULL::bigint;
        RETURN;
    END IF;
    IF NOT v_has_head THEN
        RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
        RETURN;
    END IF;
    IF v_game <> p_game OR v_rules <> p_rules_version THEN
        RETURN QUERY SELECT 'rules_mismatch'::text, NULL::text, NULL::bigint, NULL::bigint;
        RETURN;
    END IF;
    IF v_state = 'sealed' THEN
        RETURN QUERY SELECT 'rejected'::text, 'round_sealed'::text, NULL::bigint, NULL::bigint;
        RETURN;
    END IF;
    IF v_state = 'closed' AND p_class = 'ordinary' THEN
        RETURN QUERY SELECT 'rejected'::text, 'round_closed'::text, NULL::bigint, NULL::bigint;
        RETURN;
    END IF;

    -- 3. Lock the round's admission head.
    SELECT h.admitted_sequence, h.admission_state, h.close_not_before_unix_micro
        INTO v_admitted, v_state, v_bound
        FROM orvex.round_admission_heads h
        WHERE h.round_id = p_round_id
        FOR UPDATE;
    -- 4. Clock, the first statement after the lock (ADR-002).
    v_clock := orvex.unix_micro(clock_timestamp());
    IF v_admitted IS NULL THEN
        RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
        RETURN;
    END IF;

    -- 5. Clamp to the previous admission's effective time (S-7).
    v_t := v_clock;
    IF v_admitted > 0 THEN
        SELECT a.effective_time_unix_micro INTO v_prev_t
            FROM orvex.admissions a
            WHERE a.round_id = p_round_id AND a.sequence = v_admitted;
        IF NOT FOUND THEN
            RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
        v_t := greatest(v_t, v_prev_t);
    END IF;

    -- 6. Dedupe again, on a fresh snapshot.
    SELECT a.round_id, a.sequence, a.effective_time_unix_micro, a.command
        INTO v_found_round, v_found_seq, v_found_time, v_found_command
        FROM orvex.admissions a
        WHERE a.command_id = p_command_id;
    IF FOUND THEN
        IF v_found_round = p_round_id AND v_found_command = p_command THEN
            RETURN QUERY SELECT 'duplicate'::text, NULL::text, v_found_seq, v_found_time;
        ELSE
            RETURN QUERY SELECT 'conflict'::text, NULL::text, NULL::bigint, NULL::bigint;
        END IF;
        RETURN;
    END IF;

    -- 7. Window (S-8) on the locked state.
    IF v_state = 'sealed' THEN
        RETURN QUERY SELECT 'rejected'::text, 'round_sealed'::text, NULL::bigint, NULL::bigint;
        RETURN;
    ELSIF v_state = 'closed' THEN
        IF p_class = 'ordinary' THEN
            RETURN QUERY SELECT 'rejected'::text, 'round_closed'::text, NULL::bigint, NULL::bigint;
            RETURN;
        ELSIF p_class <> 'settle' THEN
            RETURN QUERY SELECT 'rejected'::text, 'lifecycle_order'::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
    ELSIF v_state = 'started' THEN
        IF p_class IN ('start', 'settle') THEN
            RETURN QUERY SELECT 'rejected'::text, 'lifecycle_order'::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
    ELSIF v_state = 'open' THEN
        IF p_class IN ('close', 'settle') THEN
            RETURN QUERY SELECT 'rejected'::text, 'lifecycle_order'::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
    ELSE
        RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
        RETURN;
    END IF;

    -- 8. Closure bound (LBT-12 engineer addendum). The offset and the
    -- bound are secret until the crash: no outcome, message or error
    -- carries them.
    IF p_class = 'start' THEN
        SELECT s.close_offset_micros INTO v_offset
            FROM orvex.round_secrets s
            WHERE s.round_id = p_round_id;
        IF v_offset IS NULL THEN  -- no secret row, or no offset recorded
            RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
        -- Saturate instead of overflowing; Decide rejects such a start anyway (D-7).
        IF v_t > 9223372036854775807 - v_offset THEN
            v_bound := 9223372036854775807;
        ELSE
            v_bound := v_t + v_offset;
        END IF;
    ELSIF p_class = 'close' THEN
        -- Step 7 admits a close only in state 'started'.
        IF v_bound IS NULL THEN
            RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
        IF v_t < v_bound THEN
            RETURN QUERY SELECT 'not_due'::text, NULL::text, NULL::bigint, v_t;
            RETURN;
        END IF;
    END IF;

    -- 9. Hold check (S-11): the account row is the only lock after the head.
    IF p_hold_player_id IS NOT NULL THEN
        SELECT la.account_id, la.credits_posted - la.debits_posted - la.held
            INTO v_account, v_available
            FROM orvex.ledger_accounts la
            WHERE la.kind = 'player' AND la.player_id = p_hold_player_id
            FOR NO KEY UPDATE;
        IF NOT FOUND OR v_available < p_hold_credits THEN
            RETURN QUERY SELECT 'rejected'::text, 'insufficient_credits'::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
    END IF;

    -- 10. Insert the admission.
    v_seq := v_admitted + 1;
    INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
        VALUES (p_round_id, v_seq, p_command_id, v_t, p_command);

    -- 11. Insert the hold; ledger_holds_apply adds it to the account's held.
    IF p_hold_player_id IS NOT NULL THEN
        INSERT INTO orvex.ledger_holds (round_id, sequence, account_id, amount)
            VALUES (p_round_id, v_seq, v_account, p_hold_credits);
    END IF;

    -- 12. Advance the head and, for a lifecycle class, the window and, for
    -- start, the closure bound.
    UPDATE orvex.round_admission_heads h
        SET admitted_sequence = v_seq,
            admission_state = CASE p_class
                WHEN 'start' THEN 'started'
                WHEN 'close' THEN 'closed'
                WHEN 'settle' THEN 'sealed'
                ELSE h.admission_state END,
            started_sequence = CASE WHEN p_class = 'start' THEN v_seq ELSE h.started_sequence END,
            closed_sequence = CASE WHEN p_class = 'close' THEN v_seq ELSE h.closed_sequence END,
            sealed_sequence = CASE WHEN p_class = 'settle' THEN v_seq ELSE h.sealed_sequence END,
            close_not_before_unix_micro = CASE WHEN p_class = 'start' THEN v_bound
                ELSE h.close_not_before_unix_micro END
        WHERE h.round_id = p_round_id;

    -- 13. Admitted.
    RETURN QUERY SELECT 'admitted'::text, NULL::text, v_seq, v_t;
END
$$;

REVOKE ALL ON FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, integer, integer) FROM PUBLIC;

COMMENT ON FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, integer, integer) IS 'Transaction A (SECURITY DEFINER): dedupe, admission window, closure bound recorded at start and enforced at close, effective time read after the round head lock, credit hold and admission insert; one row per call (ADR-002, ADR-004, ADR-005).';
