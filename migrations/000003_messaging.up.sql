CREATE TABLE outbox (
    event_id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    sequence BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    aggregate_id UUID NOT NULL,
    event_type TEXT NOT NULL CHECK (event_type IN ('WagerTransactionProcessed', 'WagerTransactionRejected', 'WalletBalanceChanged', 'WagerTransactionPendingReference')),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    occurred_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    lease_id UUID,
    lease_until TIMESTAMPTZ,
    CHECK ((lease_id IS NULL) = (lease_until IS NULL)),
    CHECK (published_at IS NULL OR (lease_id IS NULL AND lease_until IS NULL)),
    CHECK (payload->>'eventId' IS NOT NULL AND payload->>'eventId' = event_id::TEXT),
    CHECK (payload->>'eventType' IS NOT NULL AND payload->>'eventType' = event_type),
    CHECK (payload->>'aggregateId' IS NOT NULL AND payload->>'aggregateId' = aggregate_id::TEXT)
);

CREATE INDEX outbox_pending_idx ON outbox (next_attempt_at, event_id) WHERE published_at IS NULL;
-- A sequência permite publicar uma carteira por vez sem bloquear eventos de outras carteiras.
CREATE INDEX outbox_wallet_sequence_idx ON outbox (wallet_id, sequence) WHERE published_at IS NULL;

CREATE TABLE inbox (
    consumer_name TEXT NOT NULL CHECK (octet_length(consumer_name) BETWEEN 1 AND 255),
    message_id TEXT NOT NULL CHECK (octet_length(message_id) BETWEEN 1 AND 255),
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    transaction_id UUID REFERENCES wager_transactions (id),
    received_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (consumer_name, message_id),
    CHECK (completed_at IS NULL OR completed_at >= received_at),
    CHECK ((completed_at IS NULL) = (transaction_id IS NULL))
);

-- O payload deve continuar igual nas republicações, inclusive quando o saldo atual já mudou.
CREATE FUNCTION protect_outbox() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'outbox events must be retained' USING ERRCODE = '23514';
    END IF;
    IF ROW(NEW.event_id, NEW.wallet_id, NEW.sequence, NEW.aggregate_id, NEW.event_type, NEW.payload, NEW.occurred_at)
        IS DISTINCT FROM ROW(OLD.event_id, OLD.wallet_id, OLD.sequence, OLD.aggregate_id, OLD.event_type, OLD.payload, OLD.occurred_at)
        OR (OLD.published_at IS NOT NULL AND NEW IS DISTINCT FROM OLD) THEN
        RAISE EXCEPTION 'outbox event snapshot is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER outbox_no_update_payload BEFORE UPDATE OR DELETE ON outbox FOR EACH ROW EXECUTE FUNCTION protect_outbox();
CREATE TRIGGER outbox_no_truncate BEFORE TRUNCATE ON outbox FOR EACH STATEMENT EXECUTE FUNCTION protect_outbox();

CREATE FUNCTION protect_inbox() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'inbox identities must be retained' USING ERRCODE = '23514';
    END IF;
    IF ROW(NEW.consumer_name, NEW.message_id, NEW.payload_hash, NEW.received_at)
        IS DISTINCT FROM ROW(OLD.consumer_name, OLD.message_id, OLD.payload_hash, OLD.received_at)
        OR (OLD.completed_at IS NOT NULL AND NEW IS DISTINCT FROM OLD) THEN
        RAISE EXCEPTION 'inbox identity and completed result are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER inbox_no_change_identity BEFORE UPDATE OR DELETE ON inbox FOR EACH ROW EXECUTE FUNCTION protect_inbox();
CREATE TRIGGER inbox_no_truncate BEFORE TRUNCATE ON inbox FOR EACH STATEMENT EXECUTE FUNCTION protect_inbox();

-- Uma inbox incompleta não pode ser confirmada sem um responsável durável pela continuação.
CREATE FUNCTION check_inbox_completion() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE complete TIMESTAMPTZ;
BEGIN
    SELECT completed_at INTO complete FROM inbox WHERE consumer_name = NEW.consumer_name AND message_id = NEW.message_id;
    IF complete IS NULL THEN RAISE EXCEPTION 'inbox treatment must be completed before commit' USING ERRCODE = '23514'; END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER inbox_completion_guard AFTER INSERT OR UPDATE ON inbox
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_inbox_completion();
