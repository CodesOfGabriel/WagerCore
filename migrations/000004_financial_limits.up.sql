ALTER TABLE wager_transactions ADD COLUMN correlation_id UUID;
ALTER TABLE wager_transactions ADD COLUMN causation_id TEXT;

ALTER TABLE wager_transactions DROP CONSTRAINT wager_result_check;
-- Limites monetários e de versão são rejeições persistentes, para que um replay não mude o resultado.
ALTER TABLE wager_transactions ADD CONSTRAINT wager_result_check CHECK (
    (status IN ('PENDING', 'PENDING_REFERENCE') AND failure_code IS NULL AND result_balance_minor IS NULL AND resolved_reference_id IS NULL)
    OR (status = 'PROCESSED' AND failure_code IS NULL AND result_balance_minor IS NOT NULL)
    OR (status = 'REJECTED' AND failure_code IS NOT NULL AND result_balance_minor IS NOT NULL
        AND failure_code IN ('INSUFFICIENT_FUNDS', 'REVERSAL_INSUFFICIENT_FUNDS', 'REFERENCE_NOT_FOUND', 'REFERENCE_NOT_PROCESSED', 'REFERENCE_MISMATCH', 'ALREADY_REVERSED', 'AMOUNT_MISMATCH', 'BALANCE_OVERFLOW', 'WALLET_VERSION_EXHAUSTED'))
    OR (status = 'FAILED' AND failure_code IS NOT NULL AND failure_code = 'INFRASTRUCTURE_FAILURE' AND result_balance_minor IS NULL)
);
