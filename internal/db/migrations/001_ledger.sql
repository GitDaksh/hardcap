-- Hardcap's ledger. Every money column is in micro-dollars: $1 = 1,000,000.

CREATE TABLE projects (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            TEXT        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    -- 'hardcap' sets money aside before every request. 'naive' checks the
    -- balance first and charges afterwards, the way most apps do it. Naive
    -- exists only to show why that breaks under concurrent requests.
    mode            TEXT        NOT NULL DEFAULT 'hardcap' CHECK (mode IN ('hardcap', 'naive')),
    cap_micros      BIGINT      NOT NULL CHECK (cap_micros >= 0),
    spent_micros    BIGINT      NOT NULL DEFAULT 0 CHECK (spent_micros >= 0),
    reserved_micros BIGINT      NOT NULL DEFAULT 0 CHECK (reserved_micros >= 0),
    requests        BIGINT      NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id      BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name            TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    prefix          TEXT        NOT NULL,         -- the first few characters, safe to display
    secret_hash     BYTEA       NOT NULL UNIQUE,  -- SHA-256 of the key; the key itself is never stored
    cap_micros      BIGINT      CHECK (cap_micros >= 0), -- NULL: only the project's cap applies
    spent_micros    BIGINT      NOT NULL DEFAULT 0 CHECK (spent_micros >= 0),
    reserved_micros BIGINT      NOT NULL DEFAULT 0 CHECK (reserved_micros >= 0),
    rpm_limit       INTEGER     CHECK (rpm_limit >= 0), -- NULL: the default from hardcap.yaml; 0: no limit
    tpm_limit       INTEGER     CHECK (tpm_limit >= 0),
    revoked_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_project_idx ON api_keys (project_id);

-- A hold is money set aside for one request while it runs.
CREATE TABLE holds (
    request_id    TEXT        PRIMARY KEY,
    key_id        BIGINT      NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    project_id    BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    amount_micros BIGINT      NOT NULL CHECK (amount_micros >= 0),
    model         TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX holds_expires_idx ON holds (expires_at);

-- One row per finished request: the receipt behind every number.
CREATE TABLE usage_events (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id        TEXT        NOT NULL UNIQUE, -- makes settling idempotent
    key_id            BIGINT      NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    project_id        BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    model             TEXT        NOT NULL,
    provider          TEXT        NOT NULL DEFAULT '',
    outcome           TEXT        NOT NULL,
    prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
    completion_tokens INTEGER     NOT NULL DEFAULT 0,
    usage_known       BOOLEAN     NOT NULL DEFAULT false,
    held_micros       BIGINT      NOT NULL DEFAULT 0,
    cost_micros       BIGINT      NOT NULL CHECK (cost_micros >= 0),
    duration_ms       INTEGER     NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX usage_events_project_idx ON usage_events (project_id, id DESC);

-- LOCK ORDER. Every function below that locks rows locks the API key first
-- and the project second. Because everyone takes locks in the same order,
-- two requests can never end up waiting on each other forever (a deadlock).
--
-- LOCK STRENGTH. Rows are locked FOR NO KEY UPDATE, not FOR UPDATE. Writing
-- a hold or a receipt makes Postgres check its foreign keys, which takes a
-- light FOR KEY SHARE lock on the key and project rows. FOR UPDATE conflicts
-- with that lock and caused real deadlocks under load (the stress tests
-- caught it). FOR NO KEY UPDATE, the same lock a normal UPDATE takes, doesn't.

-- hardcap_reserve sets money aside for one request, but only if it fits
-- under both the key's cap and the project's cap. The check and the update
-- happen in one function, in one round trip, while holding the row locks, so
-- no other request can slip in between "check" and "spend". That gap is the
-- whole bug in naive budget code.
CREATE FUNCTION hardcap_reserve(
    p_request_id  TEXT,
    p_key_id      BIGINT,
    p_amount      BIGINT,
    p_model       TEXT,
    p_ttl_seconds DOUBLE PRECISION
) RETURNS TABLE (out_status TEXT, out_cap BIGINT, out_spent BIGINT, out_reserved BIGINT)
LANGUAGE plpgsql AS $$
DECLARE
    v_key     api_keys%ROWTYPE;
    v_project projects%ROWTYPE;
BEGIN
    SELECT * INTO v_key FROM api_keys WHERE id = p_key_id FOR NO KEY UPDATE;
    IF NOT FOUND OR v_key.revoked_at IS NOT NULL THEN
        RETURN QUERY SELECT 'invalid_key', 0::BIGINT, 0::BIGINT, 0::BIGINT;
        RETURN;
    END IF;
    SELECT * INTO v_project FROM projects WHERE id = v_key.project_id FOR NO KEY UPDATE;

    IF v_key.cap_micros IS NOT NULL
       AND v_key.spent_micros + v_key.reserved_micros + p_amount > v_key.cap_micros THEN
        RETURN QUERY SELECT 'key_cap', v_key.cap_micros, v_key.spent_micros, v_key.reserved_micros;
        RETURN;
    END IF;
    IF v_project.spent_micros + v_project.reserved_micros + p_amount > v_project.cap_micros THEN
        RETURN QUERY SELECT 'project_cap', v_project.cap_micros, v_project.spent_micros, v_project.reserved_micros;
        RETURN;
    END IF;

    UPDATE api_keys SET reserved_micros = reserved_micros + p_amount WHERE id = v_key.id;
    UPDATE projects SET reserved_micros = reserved_micros + p_amount WHERE id = v_project.id;
    INSERT INTO holds (request_id, key_id, project_id, amount_micros, model, expires_at)
    VALUES (p_request_id, v_key.id, v_project.id, p_amount, p_model,
            now() + make_interval(secs => p_ttl_seconds));

    RETURN QUERY SELECT 'ok', v_project.cap_micros, v_project.spent_micros,
                        v_project.reserved_micros + p_amount;
END;
$$;

-- hardcap_settle finishes a request: it releases the hold and charges what
-- the request really cost. It returns 'settled', or 'duplicate' if this
-- request was already settled, so calling it twice (say, a retry after a
-- network blip) can never charge twice.
CREATE FUNCTION hardcap_settle(
    p_request_id        TEXT,
    p_key_id            BIGINT,
    p_project_id        BIGINT,
    p_model             TEXT,
    p_provider          TEXT,
    p_outcome           TEXT,
    p_prompt_tokens     INTEGER,
    p_completion_tokens INTEGER,
    p_usage_known       BOOLEAN,
    p_cost              BIGINT,
    p_duration_ms       INTEGER
) RETURNS TEXT
LANGUAGE plpgsql AS $$
DECLARE
    v_held BIGINT;
BEGIN
    -- 1. Take the hold first. If the sweeper is charging this same hold
    --    right now, this waits for it, then finds the hold gone.
    DELETE FROM holds WHERE request_id = p_request_id RETURNING amount_micros INTO v_held;

    -- 2. Write the receipt. request_id is unique, so a second settle for the
    --    same request inserts nothing.
    INSERT INTO usage_events (request_id, key_id, project_id, model, provider, outcome,
                              prompt_tokens, completion_tokens, usage_known,
                              held_micros, cost_micros, duration_ms)
    VALUES (p_request_id, p_key_id, p_project_id, p_model, p_provider, p_outcome,
            p_prompt_tokens, p_completion_tokens, p_usage_known,
            COALESCE(v_held, 0), p_cost, p_duration_ms)
    ON CONFLICT (request_id) DO NOTHING;
    IF NOT FOUND THEN
        IF v_held IS NOT NULL THEN
            -- Can't happen: a hold and a receipt never exist together.
            RAISE EXCEPTION 'hold % exists for an already-settled request', p_request_id;
        END IF;
        RETURN 'duplicate';
    END IF;

    -- 3. Swap the hold for the real cost: key first, then project.
    UPDATE api_keys
       SET reserved_micros = reserved_micros - COALESCE(v_held, 0),
           spent_micros    = spent_micros + p_cost
     WHERE id = p_key_id;
    UPDATE projects
       SET reserved_micros = reserved_micros - COALESCE(v_held, 0),
           spent_micros    = spent_micros + p_cost,
           requests        = requests + 1
     WHERE id = p_project_id;
    RETURN 'settled';
END;
$$;

-- hardcap_capture_expired charges ONE expired hold in full and returns its
-- request ID, or NULL when none are left. A hold only expires when the
-- gateway that owned it died mid-request. The provider may have generated
-- every token it was allowed to, so the hold is charged, not released:
-- over-charging the ledger is safe, under-charging could break the cap.
-- SKIP LOCKED lets several gateways sweep at once without blocking each other.
CREATE FUNCTION hardcap_capture_expired() RETURNS TEXT
LANGUAGE plpgsql AS $$
DECLARE
    v_hold holds%ROWTYPE;
BEGIN
    DELETE FROM holds
     WHERE request_id = (SELECT request_id FROM holds
                          WHERE expires_at < now()
                          ORDER BY expires_at
                          LIMIT 1
                          FOR UPDATE SKIP LOCKED)
    RETURNING * INTO v_hold;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    INSERT INTO usage_events (request_id, key_id, project_id, model, outcome, usage_known,
                              held_micros, cost_micros, duration_ms)
    VALUES (v_hold.request_id, v_hold.key_id, v_hold.project_id, v_hold.model, 'expired', false,
            v_hold.amount_micros, v_hold.amount_micros,
            LEAST(extract(epoch FROM now() - v_hold.created_at) * 1000, 2147483647)::INTEGER);

    UPDATE api_keys
       SET reserved_micros = reserved_micros - v_hold.amount_micros,
           spent_micros    = spent_micros + v_hold.amount_micros
     WHERE id = v_hold.key_id;
    UPDATE projects
       SET reserved_micros = reserved_micros - v_hold.amount_micros,
           spent_micros    = spent_micros + v_hold.amount_micros,
           requests        = requests + 1
     WHERE id = v_hold.project_id;
    RETURN v_hold.request_id;
END;
$$;
