-- 0009 close_bound: restore the LBT-11 admit_command and drop the close bound, refusing when round_admission_heads or round_secrets holds rows (LBT-12 spec E-16 and engineer addendum; LBT-11 spec S-8)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orvex.round_admission_heads) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_admission_heads holds rows' USING ERRCODE = 'OX003';
    END IF;
    IF EXISTS (SELECT 1 FROM orvex.round_secrets) THEN
        RAISE EXCEPTION 'down migration refused: orvex.round_secrets holds rows' USING ERRCODE = 'OX003';
    END IF;
END
$$;

DROP FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, integer, integer);

-- Restore 0007's admit_command verbatim (LBT-11 spec S-8, §5.3).
-- admit_command is Transaction A's single statement (S-3, §5.3). Steps 1
-- and 2 are the lock-free fast path (S-5); steps 3 to 13 run under the
-- round's head lock, which the caller's COMMIT or ROLLBACK releases. Every
-- column reference is qualified: the RETURNS TABLE columns sequence and
-- effective_time_unix_micro are also PL/pgSQL variables. The function never
-- catches exceptions.
CREATE FUNCTION orvex.admit_command(
    p_round_id              uuid,
    p_game                  text,
    p_rules_version         bigint,
    p_command_id            uuid,
    p_command               bytea,
    p_class                 text,
    p_hold_player_id        text,
    p_hold_credits          bigint,
    p_not_before_unix_micro bigint,
    p_lock_timeout_ms       integer,
    p_idle_timeout_ms       integer)
    RETURNS TABLE (outcome text, reason text, sequence bigint, effective_time_unix_micro bigint)
    LANGUAGE plpgsql VOLATILE
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
    v_started       bigint;
    v_clock         bigint;
    v_t             bigint;
    v_prev_t        bigint;
    v_t0            bigint;
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
    IF (p_class = 'close') <> (p_not_before_unix_micro IS NOT NULL) THEN
        RAISE EXCEPTION 'orvex.admit_command: not-before required exactly for class close' USING ERRCODE = '22023';
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
    SELECT h.admitted_sequence, h.admission_state, h.started_sequence
        INTO v_admitted, v_state, v_started
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

    -- 8. Closure guard.
    IF p_class = 'close' THEN
        SELECT a.effective_time_unix_micro INTO v_t0
            FROM orvex.admissions a
            WHERE a.round_id = p_round_id AND a.sequence = v_started;
        IF NOT FOUND THEN
            RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
        IF p_not_before_unix_micro < v_t0 THEN
            RETURN QUERY SELECT 'invalid_not_before'::text, NULL::text, NULL::bigint, NULL::bigint;
            RETURN;
        END IF;
        IF v_t < p_not_before_unix_micro THEN
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

    -- 12. Advance the head and, for a lifecycle class, the window.
    UPDATE orvex.round_admission_heads h
        SET admitted_sequence = v_seq,
            admission_state = CASE p_class
                WHEN 'start' THEN 'started'
                WHEN 'close' THEN 'closed'
                WHEN 'settle' THEN 'sealed'
                ELSE h.admission_state END,
            started_sequence = CASE WHEN p_class = 'start' THEN v_seq ELSE h.started_sequence END,
            closed_sequence = CASE WHEN p_class = 'close' THEN v_seq ELSE h.closed_sequence END,
            sealed_sequence = CASE WHEN p_class = 'settle' THEN v_seq ELSE h.sealed_sequence END
        WHERE h.round_id = p_round_id;

    -- 13. Admitted.
    RETURN QUERY SELECT 'admitted'::text, NULL::text, v_seq, v_t;
END
$$;

COMMENT ON FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, bigint, integer, integer) IS 'Transaction A: dedupe, admission window and closure barrier, effective time read after the round head lock, credit hold and admission insert; one row per call (ADR-002, ADR-005).';

DROP TRIGGER round_admission_heads_close_bound ON orvex.round_admission_heads;
DROP FUNCTION orvex.check_close_bound();

ALTER TABLE orvex.round_admission_heads
    DROP CONSTRAINT round_admission_heads_close_bound_check,
    DROP COLUMN close_not_before_unix_micro;

ALTER TABLE orvex.round_secrets
    DROP CONSTRAINT round_secrets_close_offset_check,
    DROP COLUMN close_offset_micros;
