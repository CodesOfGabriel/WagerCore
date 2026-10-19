# API

Os exemplos de inicialização e obtenção de tokens estão no [README](../README.md). Todas as respostas de negócio incluem `X-Correlation-Id`. Os corpos JSON aceitam até 64 KiB e rejeitam campos desconhecidos. Valores monetários usam `{"amount":"25.00","currency":"BRL"}`.

## Permissões

| Cliente | Permissão |
| --- | --- |
| `internal-service` | Abrir e consultar carteira, ler ledger, reconciliar e ler métricas |
| `provider-a` e `provider-b` | Enviar e consultar suas próprias transações |

Use `Authorization: Bearer TOKEN`. O `providerId` enviado precisa coincidir com a identidade do token. Um provedor não pode ler transações do outro, mesmo conhecendo o UUID ou ID externo. Aberturas internas também não ficam disponíveis pela consulta de provedores.

Os clients locais usam secrets no formato `CLIENT-local-secret`. O script `scripts/token.sh` aceita o nome do client e permite sobrescrever `CLIENT_SECRET` e `OIDC_ISSUER_URL`. O realm contém ainda clients de teste de expiração e audience, utilizados pela integração.

## Carteiras

`POST /wallets`, com token interno:

```json
{
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "initialBalance": {"amount":"100.00","currency":"BRL"}
}
```

Retorna `201`:

```json
{
  "id":"0192f291-27dd-7d3f-8071-5f8685deef37",
  "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "balance":{"amount":"100.00","currency":"BRL"},
  "version":1
}
```

`GET /wallets/{walletId}` retorna o mesmo formato com saldo e versão atuais. Uma segunda abertura para o mesmo jogador e moeda retorna `409`. Zero é permitido na abertura; valores negativos são inválidos.

## Operações

`POST /wagering/transactions`, com token do provedor e um único header `Idempotency-Key`:

```json
{
  "providerId":"provider-a",
  "externalTransactionId":"bet-001",
  "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
  "roundId":"round-1",
  "gameId":"game-1",
  "kind":"BET",
  "money":{"amount":"25.00","currency":"BRL"}
}
```

O resultado processado usa `200`:

```json
{
  "transactionId":"0192f298-345e-7e38-af88-e43f851a819d",
  "status":"PROCESSED",
  "balance":{"amount":"75.00","currency":"BRL"},
  "idempotentReplay":false
}
```

`WIN`, `BET`, `REFUND` e `ROLLBACK` exigem valor positivo. `LOSS` exige `0.00`. Reversões exigem `referenceExternalTransactionId`; `WIN` pode informar uma BET da mesma rodada. `OPENING` é reservado à abertura de carteira e não é aceito nessa rota.

Uma rejeição de negócio usa `422`, sem lançamento financeiro:

```json
{
  "transactionId":"0192f298-345e-7e38-af88-e43f851a819d",
  "status":"REJECTED",
  "balance":{"amount":"20.00","currency":"BRL"},
  "failureCode":"INSUFFICIENT_FUNDS",
  "idempotentReplay":false
}
```

Uma referência ausente usa `202`. Enquanto estiver pendente, o resultado não contém saldo:

```json
{
  "transactionId":"0192f298-345e-7e38-af88-e43f851a819d",
  "status":"PENDING_REFERENCE",
  "idempotentReplay":false
}
```

Reenviar a mesma identidade e conteúdo retorna `idempotentReplay: true`, com o status e resultado persistidos. Uma rejeição continua usando `422` no replay; uma pendência continua usando `202` até sua resolução. Reutilizar a chave com outro conteúdo ou o ID externo com outra chave retorna `409`. O algoritmo de hash e as normalizações estão na arquitetura.

| Código de rejeição | Significado |
| --- | --- |
| `INSUFFICIENT_FUNDS` | BET sem saldo |
| `REVERSAL_INSUFFICIENT_FUNDS` | Rollback de crédito sem saldo disponível |
| `REFERENCE_NOT_FOUND` | Prazo ou tentativas para a referência esgotados |
| `REFERENCE_NOT_PROCESSED` | Referência encerrada sem sucesso |
| `REFERENCE_MISMATCH` | Identidade, rodada ou tipo da referência incompatível |
| `AMOUNT_MISMATCH` | Valor diferente do total da operação revertida |
| `ALREADY_REVERSED` | Referência já revertida com sucesso |
| `BALANCE_OVERFLOW` | Crédito ultrapassaria o limite de centavos |
| `WALLET_VERSION_EXHAUSTED` | Movimento ultrapassaria o limite da versão |

## Consultas e paginação

- `GET /wagering/transactions/{transactionId}`
- `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}`

As consultas retornam ID interno, campos de negócio, status, saldo quando resolvido, código de falha quando presente e timestamps. Não expõem hash ou chave de idempotência. Transação inexistente ou de outro provedor retorna `404`.

`GET /wallets/{walletId}/ledger?limit=50&cursor=...` exige token interno. `limit` aceita de 1 a 100. A ordem é crescente por `createdAt` e `id`. A resposta contém `entries` e, quando houver continuação, `nextCursor`. O cliente deve reenviar o cursor sem alterá-lo.

Cada entrada contém `id`, `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter`, `walletVersion` e `createdAt`. Uma página vazia retorna `entries: []`; cursor inválido retorna `400`.

## Reconciliação

`POST /wallets/{walletId}/reconciliation`, com token interno e sem corpo:

```json
{
  "walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
  "storedBalance":{"amount":"75.00","currency":"BRL"},
  "calculatedBalance":{"amount":"75.00","currency":"BRL"},
  "difference":{"amount":"0.00","currency":"BRL"},
  "consistent":true,
  "checkedEntries":2
}
```

A resposta usa `200` mesmo quando `consistent` for `false`, pois a consulta foi concluída. Divergências não alteram o saldo.

## Erros HTTP

| HTTP | Corpo ou código | Uso |
| --- | --- | --- |
| `400` | `INVALID_REQUEST`, com `message` | JSON, identificador, moeda, valor ou referência inválidos |
| `401` | `UNAUTHORIZED` | Token ausente, inválido, expirado ou com claims incompatíveis |
| `403` | `FORBIDDEN` | Papel incompatível ou tentativa de enviar por outro provedor |
| `404` | `NOT_FOUND` | Recurso inexistente ou transação inacessível ao provedor |
| `409` | `CONFLICT` | Identidade reutilizada de forma incompatível ou carteira duplicada |
| `422` | Resultado `REJECTED` | Rejeição financeira persistida |
| `202` | Resultado `PENDING_REFERENCE` | Continuação durável aguardando referência |
| `503` | `TEMPORARILY_UNAVAILABLE` | Dependência indisponível ou prazo excedido; `Retry-After: 1` |
| `500` | `INTERNAL_ERROR` | Falha não classificada |

Erros que não são resultados financeiros usam `{"code":"CODIGO"}`. Entrada inválida também traz uma mensagem curta. Detalhes de banco e credenciais não são devolvidos.

## Entrada por SQS

Envie para `wager-transactions.fifo`:

```json
{
  "messageId":"request-bet-001",
  "type":"WagerTransactionRequested",
  "occurredAt":"2026-10-19T12:00:00Z",
  "data":{
    "providerId":"provider-a",
    "externalTransactionId":"bet-001",
    "idempotencyKey":"provider-a:bet-001",
    "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId":"round-1",
    "gameId":"game-1",
    "kind":"BET",
    "money":{"amount":"25.00","currency":"BRL"}
  }
}
```

Use a carteira como `MessageGroupId` e uma identidade estável da mensagem como `MessageDeduplicationId`. A resposta assíncrona é acompanhada pela consulta HTTP do provedor e pelos eventos em `integration-events.fifo`.

Para enviar um arquivo `message.json` pelo emulador local:

```sh
QUEUE_URL=$(docker compose -f docker-compose.local.yml exec -T localstack \
  awslocal sqs get-queue-url --queue-name wager-transactions.fifo --query QueueUrl --output text)

docker compose -f docker-compose.local.yml exec -T localstack sh -c \
  'cat > /tmp/wager-request.json' < message.json

docker compose -f docker-compose.local.yml exec -T localstack \
  awslocal sqs send-message --queue-url "$QUEUE_URL" \
  --message-body file:///tmp/wager-request.json \
  --message-group-id "$WALLET_ID" \
  --message-deduplication-id request-bet-001
```

A arquitetura descreve a inbox, política de retry, DLQ e entrega ao menos uma vez.
