CREATE TABLE wallets (
    id UUID PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    player_id UUID NOT NULL CHECK (player_id <> '00000000-0000-0000-0000-000000000000'),
    currency TEXT NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),
    -- Centavos em inteiro preservam a precisão sem cálculos de ponto flutuante.
    balance_minor BIGINT NOT NULL CHECK (balance_minor >= 0),
    version BIGINT NOT NULL CHECK (version >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency),
    CONSTRAINT wallets_identity_key UNIQUE (id, player_id, currency)
);

CREATE TABLE wager_transactions (
    id UUID PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    wallet_id UUID NOT NULL,
    player_id UUID NOT NULL,
    currency TEXT NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    provider_id TEXT,
    external_transaction_id TEXT,
    idempotency_key TEXT,
    payload_hash BYTEA,
    round_id TEXT,
    game_id TEXT,
    reference_external_transaction_id TEXT,
    resolved_reference_id UUID REFERENCES wager_transactions (id) DEFERRABLE INITIALLY DEFERRED,
    failure_code TEXT,
    -- O replay devolve este saldo, mesmo que a carteira já tenha recebido outros movimentos.
    result_balance_minor BIGINT CHECK (result_balance_minor >= 0),
    reference_attempts INTEGER NOT NULL DEFAULT 0 CHECK (reference_attempts >= 0),
    next_retry_at TIMESTAMPTZ,
    reference_expires_at TIMESTAMPTZ,
    -- A identidade da reserva impede um worker antigo de confirmar trabalho reassumido por outro.
    lease_id UUID,
    lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    CONSTRAINT wager_wallet_identity_fk FOREIGN KEY (wallet_id, player_id, currency)
        REFERENCES wallets (id, player_id, currency),
    CONSTRAINT wager_provider_idempotency_key UNIQUE (provider_id, idempotency_key),
    -- Outra chave de idempotência não pode transformar o mesmo ID externo em uma nova operação.
    CONSTRAINT wager_provider_external_key UNIQUE (provider_id, external_transaction_id),
    CONSTRAINT wager_ledger_identity_key UNIQUE (id, wallet_id, currency, amount_minor),
    CONSTRAINT wager_amount_check CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),
    -- A abertura é interna; exigir metadados de provedor aqui criaria identidades externas fictícias.
    CONSTRAINT wager_origin_check CHECK (
        (kind = 'OPENING' AND status = 'PROCESSED'
            AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL AND resolved_reference_id IS NULL)
        OR
        (kind <> 'OPENING'
            AND provider_id IS NOT NULL AND octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id = btrim(provider_id)
            AND external_transaction_id IS NOT NULL AND octet_length(external_transaction_id) BETWEEN 1 AND 255 AND external_transaction_id = btrim(external_transaction_id)
            AND idempotency_key IS NOT NULL AND octet_length(idempotency_key) BETWEEN 1 AND 255 AND idempotency_key = btrim(idempotency_key)
            AND payload_hash IS NOT NULL AND octet_length(payload_hash) = 32
            AND round_id IS NOT NULL AND octet_length(round_id) BETWEEN 1 AND 255 AND round_id = btrim(round_id)
            AND game_id IS NOT NULL AND octet_length(game_id) BETWEEN 1 AND 255 AND game_id = btrim(game_id))
    ),
    CONSTRAINT wager_reference_check CHECK (
        (reference_external_transaction_id IS NULL OR
            (octet_length(reference_external_transaction_id) BETWEEN 1 AND 255
                AND reference_external_transaction_id = btrim(reference_external_transaction_id)
                AND reference_external_transaction_id <> external_transaction_id))
        AND (kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_transaction_id IS NOT NULL)
        AND (kind NOT IN ('BET', 'LOSS', 'OPENING') OR reference_external_transaction_id IS NULL)
        AND (resolved_reference_id IS NULL OR (reference_external_transaction_id IS NOT NULL AND resolved_reference_id <> id))
        AND (status <> 'PROCESSED' OR (reference_external_transaction_id IS NULL) = (resolved_reference_id IS NULL))
    ),
    CONSTRAINT wager_result_check CHECK (
        (status IN ('PENDING', 'PENDING_REFERENCE') AND failure_code IS NULL AND result_balance_minor IS NULL AND resolved_reference_id IS NULL)
        OR (status = 'PROCESSED' AND failure_code IS NULL AND result_balance_minor IS NOT NULL)
        OR (status = 'REJECTED' AND failure_code IS NOT NULL AND result_balance_minor IS NOT NULL
            AND failure_code IN ('INSUFFICIENT_FUNDS', 'REVERSAL_INSUFFICIENT_FUNDS', 'REFERENCE_NOT_FOUND', 'REFERENCE_NOT_PROCESSED', 'REFERENCE_MISMATCH', 'ALREADY_REVERSED', 'AMOUNT_MISMATCH'))
        OR (status = 'FAILED' AND failure_code IS NOT NULL AND failure_code = 'INFRASTRUCTURE_FAILURE' AND result_balance_minor IS NULL)
    ),
    CONSTRAINT wager_retry_check CHECK (
        (status = 'PENDING_REFERENCE' AND reference_external_transaction_id IS NOT NULL
            AND next_retry_at IS NOT NULL AND reference_expires_at IS NOT NULL
            AND next_retry_at >= created_at AND reference_expires_at > created_at
            AND next_retry_at <= reference_expires_at)
        OR (status <> 'PENDING_REFERENCE' AND next_retry_at IS NULL AND reference_expires_at IS NULL)
    ),
    CONSTRAINT wager_lease_check CHECK (
        (lease_id IS NULL AND lease_until IS NULL)
        OR (status = 'PENDING_REFERENCE' AND lease_id IS NOT NULL AND lease_until IS NOT NULL)
    )
);

CREATE UNIQUE INDEX wager_opening_wallet_key ON wager_transactions (wallet_id) WHERE kind = 'OPENING';
-- REFUND e ROLLBACK disputam a mesma referência para impedir duas devoluções do mesmo débito.
CREATE UNIQUE INDEX wager_successful_reversal_key ON wager_transactions (resolved_reference_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');
CREATE INDEX wager_pending_reference_idx ON wager_transactions (next_retry_at, id) WHERE status = 'PENDING_REFERENCE';
CREATE INDEX wager_wallet_created_idx ON wager_transactions (wallet_id, created_at, id);

CREATE TABLE wallet_ledger (
    id UUID PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    wallet_id UUID NOT NULL REFERENCES wallets (id),
    transaction_id UUID NOT NULL,
    currency TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    balance_before_minor BIGINT NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor BIGINT NOT NULL CHECK (balance_after_minor >= 0),
    wallet_version BIGINT NOT NULL CHECK (wallet_version >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT ledger_transaction_fk FOREIGN KEY (transaction_id, wallet_id, currency, amount_minor)
        REFERENCES wager_transactions (id, wallet_id, currency, amount_minor),
    CONSTRAINT ledger_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    CONSTRAINT ledger_wallet_version_key UNIQUE (wallet_id, wallet_version),
    -- NUMERIC evita overflow durante a validação; o armazenamento continua em BIGINT.
    CONSTRAINT ledger_arithmetic_check CHECK (
        (direction = 'CREDIT' AND balance_after_minor::NUMERIC = balance_before_minor::NUMERIC + amount_minor::NUMERIC)
        OR (direction = 'DEBIT' AND balance_after_minor::NUMERIC = balance_before_minor::NUMERIC - amount_minor::NUMERIC)
    )
);

CREATE INDEX ledger_wallet_cursor_idx ON wallet_ledger (wallet_id, created_at, id);

-- Correções exigem novos lançamentos para preservar o histórico usado na reconciliação.
CREATE FUNCTION protect_ledger() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallet ledger is append-only' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER ledger_no_update_delete BEFORE UPDATE OR DELETE ON wallet_ledger
    FOR EACH ROW EXECUTE FUNCTION protect_ledger();
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON wallet_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION protect_ledger();

CREATE FUNCTION protect_wallet() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.version <> 1 THEN
            RAISE EXCEPTION 'wallet initial version must be one' USING ERRCODE = '23514';
        END IF;
    ELSE
        IF ROW(NEW.id, NEW.player_id, NEW.currency, NEW.created_at)
            IS DISTINCT FROM ROW(OLD.id, OLD.player_id, OLD.currency, OLD.created_at)
            OR NEW.updated_at < OLD.updated_at THEN
            RAISE EXCEPTION 'wallet identity and creation time are immutable' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wallet_identity_guard BEFORE INSERT OR UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION protect_wallet();

CREATE FUNCTION protect_wager_transaction() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'terminal transaction is immutable' USING ERRCODE = '23514';
    END IF;
    IF ROW(NEW.id, NEW.wallet_id, NEW.player_id, NEW.currency, NEW.amount_minor, NEW.kind,
        NEW.provider_id, NEW.external_transaction_id, NEW.idempotency_key, NEW.payload_hash,
        NEW.round_id, NEW.game_id, NEW.reference_external_transaction_id, NEW.created_at)
        IS DISTINCT FROM ROW(OLD.id, OLD.wallet_id, OLD.player_id, OLD.currency, OLD.amount_minor, OLD.kind,
        OLD.provider_id, OLD.external_transaction_id, OLD.idempotency_key, OLD.payload_hash,
        OLD.round_id, OLD.game_id, OLD.reference_external_transaction_id, OLD.created_at) THEN
        RAISE EXCEPTION 'transaction input is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at OR (OLD.status = 'PENDING_REFERENCE' AND NEW.status = 'PENDING') THEN
        RAISE EXCEPTION 'invalid transaction transition' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_transition_guard BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION protect_wager_transaction();

CREATE FUNCTION check_ledger_sequence() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    previous wallet_ledger%ROWTYPE;
BEGIN
    -- O lock deve vir antes da leitura do último lançamento para coordenar processos distintos.
    PERFORM 1 FROM wallets WHERE id = NEW.wallet_id FOR UPDATE;
    SELECT * INTO previous FROM wallet_ledger WHERE wallet_id = NEW.wallet_id
        ORDER BY wallet_version DESC LIMIT 1;
    IF FOUND THEN
        IF NEW.balance_before_minor <> previous.balance_after_minor
            OR NEW.wallet_version::NUMERIC <> previous.wallet_version::NUMERIC + 1
            OR NEW.created_at < previous.created_at THEN
            RAISE EXCEPTION 'ledger sequence is inconsistent' USING ERRCODE = '23514';
        END IF;
    -- A abertura positiva usa versão 1; o primeiro movimento de uma carteira zerada usa versão 2.
    ELSIF NEW.balance_before_minor <> 0 OR NEW.wallet_version NOT IN (1, 2) THEN
        RAISE EXCEPTION 'invalid first ledger entry' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER ledger_sequence_guard BEFORE INSERT ON wallet_ledger
    FOR EACH ROW EXECUTE FUNCTION check_ledger_sequence();

CREATE FUNCTION check_wallet_consistency() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    wallet wallets%ROWTYPE;
    last_entry wallet_ledger%ROWTYPE;
    opening wager_transactions%ROWTYPE;
    target_id UUID;
BEGIN
    IF TG_TABLE_NAME = 'wallets' THEN target_id := NEW.id; ELSE target_id := NEW.wallet_id; END IF;
    SELECT * INTO wallet FROM wallets WHERE id = target_id;
    SELECT * INTO last_entry FROM wallet_ledger WHERE wallet_id = target_id
        ORDER BY wallet_version DESC LIMIT 1;
    IF wallet.balance_minor <> COALESCE(last_entry.balance_after_minor, 0)
        OR wallet.version <> COALESCE(last_entry.wallet_version, 1) THEN
        RAISE EXCEPTION 'wallet balance or version does not match ledger' USING ERRCODE = '23514';
    END IF;
    IF TG_TABLE_NAME = 'wallets' AND TG_OP = 'INSERT' THEN
        SELECT * INTO opening FROM wager_transactions WHERE wallet_id = target_id AND kind = 'OPENING';
        IF (NEW.balance_minor > 0 AND (opening.id IS NULL OR opening.amount_minor <> NEW.balance_minor))
            OR (NEW.balance_minor = 0 AND opening.id IS NOT NULL) THEN
            RAISE EXCEPTION 'wallet opening does not match initial balance' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

-- A consistência é conferida no commit, depois de gravar carteira, transação e ledger.
CREATE CONSTRAINT TRIGGER wallet_balance_guard AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wallet_consistency();
CREATE CONSTRAINT TRIGGER ledger_balance_guard AFTER INSERT ON wallet_ledger
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wallet_consistency();

CREATE FUNCTION check_transaction_consistency() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    operation wager_transactions%ROWTYPE;
    reference wager_transactions%ROWTYPE;
    entry wallet_ledger%ROWTYPE;
    expected_direction TEXT;
BEGIN
    SELECT * INTO operation FROM wager_transactions WHERE id = NEW.id;
    SELECT * INTO entry FROM wallet_ledger WHERE transaction_id = operation.id;
    IF operation.status <> 'PROCESSED' OR operation.kind = 'LOSS' THEN
        IF entry.id IS NOT NULL THEN
            RAISE EXCEPTION 'transaction cannot have a ledger entry' USING ERRCODE = '23514';
        END IF;
        RETURN NULL;
    END IF;
    IF entry.id IS NULL OR operation.result_balance_minor <> entry.balance_after_minor THEN
        RAISE EXCEPTION 'processed transaction must match its ledger result' USING ERRCODE = '23514';
    END IF;
    IF operation.kind IN ('OPENING', 'WIN', 'REFUND') THEN expected_direction := 'CREDIT';
    ELSIF operation.kind = 'BET' THEN expected_direction := 'DEBIT'; END IF;
    IF operation.resolved_reference_id IS NOT NULL THEN
        SELECT * INTO reference FROM wager_transactions WHERE id = operation.resolved_reference_id;
        IF reference.id IS NULL OR reference.status <> 'PROCESSED'
            OR ROW(operation.provider_id, operation.wallet_id, operation.player_id, operation.currency, operation.round_id, operation.reference_external_transaction_id)
                IS DISTINCT FROM ROW(reference.provider_id, reference.wallet_id, reference.player_id, reference.currency, reference.round_id, reference.external_transaction_id) THEN
            RAISE EXCEPTION 'transaction reference does not match' USING ERRCODE = '23514';
        END IF;
        IF (operation.kind IN ('WIN', 'REFUND') AND reference.kind <> 'BET')
            OR (operation.kind = 'ROLLBACK' AND reference.kind NOT IN ('BET', 'WIN', 'REFUND'))
            OR (operation.kind IN ('REFUND', 'ROLLBACK') AND operation.amount_minor <> reference.amount_minor) THEN
            RAISE EXCEPTION 'invalid reference kind or amount' USING ERRCODE = '23514';
        END IF;
        IF operation.kind = 'ROLLBACK' THEN
            IF reference.kind = 'BET' THEN expected_direction := 'CREDIT'; ELSE expected_direction := 'DEBIT'; END IF;
        END IF;
    END IF;
    IF entry.direction <> expected_direction OR expected_direction IS NULL
        OR (operation.kind = 'OPENING' AND (entry.wallet_version <> 1 OR entry.balance_before_minor <> 0))
        OR (operation.kind <> 'OPENING' AND entry.wallet_version < 2) THEN
        RAISE EXCEPTION 'ledger direction or version does not match transaction' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wager_financial_guard AFTER INSERT OR UPDATE ON wager_transactions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_transaction_consistency();

CREATE FUNCTION check_ledger_transaction() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    operation wager_transactions%ROWTYPE;
BEGIN
    SELECT * INTO operation FROM wager_transactions WHERE id = NEW.transaction_id;
    IF operation.status <> 'PROCESSED' OR operation.kind = 'LOSS' THEN
        RAISE EXCEPTION 'ledger requires a processed financial transaction' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_transaction_guard AFTER INSERT ON wallet_ledger
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_ledger_transaction();
