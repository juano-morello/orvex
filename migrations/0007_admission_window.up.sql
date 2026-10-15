-- 0007 admission_window: admission window columns, effective-time conversion and admit_command (LBT-11 spec S-7, S-8, §5)

ALTER TABLE orvex.round_admission_heads
    ADD COLUMN admission_state  text NOT NULL DEFAULT 'open',
    ADD COLUMN started_sequence bigint,
    ADD COLUMN closed_sequence  bigint,
    ADD COLUMN sealed_sequence  bigint,
    ADD CONSTRAINT round_admission_heads_state_check CHECK (
        admission_state IN ('open', 'started', 'closed', 'sealed')),
    ADD CONSTRAINT round_admission_heads_window_check CHECK (
        CASE admission_state
            WHEN 'open' THEN
                started_sequence IS NULL AND closed_sequence IS NULL AND sealed_sequence IS NULL
            WHEN 'started' THEN
                started_sequence IS NOT NULL AND closed_sequence IS NULL AND sealed_sequence IS NULL
            WHEN 'closed' THEN
                started_sequence IS NOT NULL AND closed_sequence IS NOT NULL
                AND closed_sequence > started_sequence AND sealed_sequence IS NULL
            WHEN 'sealed' THEN
                started_sequence IS NOT NULL AND closed_sequence IS NOT NULL AND sealed_sequence IS NOT NULL
                AND started_sequence < closed_sequence AND closed_sequence < sealed_sequence
                AND sealed_sequence = admitted_sequence
            -- An unknown state is round_admission_heads_state_check's to report.
            ELSE true
        END
        AND (started_sequence IS NULL OR (started_sequence >= 1 AND started_sequence <= admitted_sequence))
        AND (closed_sequence IS NULL OR (closed_sequence >= 1 AND closed_sequence <= admitted_sequence))
        AND (sealed_sequence IS NULL OR (sealed_sequence >= 1 AND sealed_sequence <= admitted_sequence))),
    ADD CONSTRAINT round_admission_heads_started_fkey FOREIGN KEY (round_id, started_sequence)
        REFERENCES orvex.admissions (round_id, sequence) NOT DEFERRABLE,
    ADD CONSTRAINT round_admission_heads_closed_fkey FOREIGN KEY (round_id, closed_sequence)
        REFERENCES orvex.admissions (round_id, sequence) NOT DEFERRABLE,
    ADD CONSTRAINT round_admission_heads_sealed_fkey FOREIGN KEY (round_id, sealed_sequence)
        REFERENCES orvex.admissions (round_id, sequence) NOT DEFERRABLE;

COMMENT ON COLUMN orvex.round_admission_heads.admission_state IS 'Admission window: open, started, closed or sealed; moves forward only under the head lock (ADR-002, ADR-005).';
COMMENT ON COLUMN orvex.round_admission_heads.started_sequence IS 'Admission sequence of the admitted system StartRound; set once (ADR-002).';
COMMENT ON COLUMN orvex.round_admission_heads.closed_sequence IS 'Admission sequence of the admitted system CloseRound, the closure barrier; set once (ADR-002, ADR-004).';
COMMENT ON COLUMN orvex.round_admission_heads.sealed_sequence IS 'Admission sequence of the admitted system SettleRound, the round''s last admission; set once (ADR-002, ADR-005).';

-- check_admission_window keeps the window forward-only (S-8): the state
-- rank never decreases and a set sequence never changes. An unknown state
-- ranks NULL and is left to round_admission_heads_state_check.
CREATE FUNCTION orvex.check_admission_window() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    v_old_rank integer;
    v_new_rank integer;
BEGIN
    v_old_rank := CASE OLD.admission_state
        WHEN 'open' THEN 0 WHEN 'started' THEN 1 WHEN 'closed' THEN 2 WHEN 'sealed' THEN 3 END;
    v_new_rank := CASE NEW.admission_state
        WHEN 'open' THEN 0 WHEN 'started' THEN 1 WHEN 'closed' THEN 2 WHEN 'sealed' THEN 3 END;
    IF (v_new_rank < v_old_rank) IS TRUE
        OR (OLD.started_sequence IS NOT NULL AND OLD.started_sequence IS DISTINCT FROM NEW.started_sequence)
        OR (OLD.closed_sequence IS NOT NULL AND OLD.closed_sequence IS DISTINCT FROM NEW.closed_sequence)
        OR (OLD.sealed_sequence IS NOT NULL AND OLD.sealed_sequence IS DISTINCT FROM NEW.sealed_sequence) THEN
        RAISE EXCEPTION 'orvex.round_admission_heads: admission window of round % moves backwards', OLD.round_id
            USING ERRCODE = 'OX005';
    END IF;
    RETURN NEW;
END
$$;

COMMENT ON FUNCTION orvex.check_admission_window() IS 'Raises OX005 when an admission window moves backwards or a set window sequence changes (ADR-002, ADR-005).';

CREATE TRIGGER round_admission_heads_window BEFORE UPDATE ON orvex.round_admission_heads
    FOR EACH ROW EXECUTE FUNCTION orvex.check_admission_window();

-- unix_micro is the only conversion of a timestamp into an effective time
-- (S-7, §5.2). It is exact at timestamptz's microsecond resolution.
CREATE FUNCTION orvex.unix_micro(ts timestamptz) RETURNS bigint
    LANGUAGE plpgsql IMMUTABLE
    SET search_path = pg_catalog
AS $$
BEGIN
    RETURN (EXTRACT(EPOCH FROM ts) * 1000000)::bigint;
END
$$;

COMMENT ON FUNCTION orvex.unix_micro(timestamptz) IS 'Microseconds since the Unix epoch of a timestamp: the only conversion into an effective admission time (ADR-002).';

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
