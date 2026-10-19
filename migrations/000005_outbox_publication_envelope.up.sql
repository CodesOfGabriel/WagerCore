-- IS TRUE rejects missing fields as well as explicit JSON nulls.
-- Existing snapshots are validated without changing their immutable payloads.
ALTER TABLE outbox ADD CONSTRAINT outbox_publication_envelope_check CHECK ((
    jsonb_typeof(payload->'correlationId') = 'string'
    AND (NOT (payload ? 'causationId') OR jsonb_typeof(payload->'causationId') = 'string')
    AND jsonb_typeof(payload->'data') = 'object'
    AND jsonb_typeof(payload->'data'->'walletId') = 'string'
    AND payload->'data'->>'walletId' = wallet_id::TEXT
    AND jsonb_typeof(payload->'data'->'transactionId') = 'string'
    AND (NOT ((payload->'data') ? 'externalTransactionId')
        OR jsonb_typeof(payload->'data'->'externalTransactionId') = 'string')
) IS TRUE);
