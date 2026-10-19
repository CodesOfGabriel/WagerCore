-- Excluir uma rejeição apagaria sua identidade e permitiria tratar um reenvio como operação nova.
CREATE FUNCTION protect_transaction_audit() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wager transactions must be retained for audit and idempotency' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER wager_no_delete BEFORE DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION protect_transaction_audit();
CREATE TRIGGER wager_no_truncate BEFORE TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION protect_transaction_audit();
