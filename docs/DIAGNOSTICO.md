# Diagnóstico de falhas

A resposta HTTP e o identificador da mensagem permitem localizar a etapa que falhou sem depender de payloads nos logs. Em requisições autenticadas, a API gera `X-Correlation-Id`; esse valor identifica os logs da requisição e fica nos eventos produzidos. Cada recebimento SQS gera uma correlação própria. A identidade financeira e a inbox continuam sendo as referências entre reentregas.

## Estado dos serviços

```sh
docker compose -f docker-compose.local.yml ps
curl -i http://localhost:8080/health/live
curl -i http://localhost:8080/health/ready
docker compose -f docker-compose.local.yml logs -f --tail=100 app
```

Liveness verifica o processo. Readiness consulta PostgreSQL e as três filas; o corpo indica qual dependência falhou. A inicialização também exige acesso às chaves do Keycloak. Para uma falha antes do início da API:

```sh
docker compose -f docker-compose.local.yml logs --tail=100 migrate postgres keycloak localstack app
```

Migrations precisam terminar com sucesso antes de a API iniciar. Divergência de checksum exige conferir o histórico e o arquivo aplicado, sem editar o checksum para contornar a falha.

## Campos dos logs

| Campo | Uso |
| --- | --- |
| `stage` | Etapa que falhou |
| `errorCategory` | `invalid_input`, `conflict`, `permanent_infrastructure`, `database`, `broker`, `network`, `timeout`, `canceled`, `unavailable`, `lease_lost`, `not_found` ou `internal` |
| `errorType` | Tipo Go da causa disponível, sem sua mensagem bruta |
| `correlationId` | Liga requisição ou recebimento aos eventos |
| `messageId` | Identidade do envelope de entrada SQS; na publicação, causação do evento quando houver |
| `sqsMessageId` | Identidade atribuída pelo broker; disponível mesmo quando o JSON é inválido |
| `transactionId`, `walletId`, `providerId`, `externalTransactionId` | Localizam a operação e seus registros quando disponíveis |
| `sqlState`, `constraint` | Código PostgreSQL e restrição envolvida |
| `service`, `sdkOperation`, `errorCode` | Serviço, operação do SDK e código retornado |
| `requestId`, `upstreamStatus` | Identificador e status da resposta do serviço, quando fornecidos |
| `receiveCount`, `attempt` | Recebimentos da mensagem ou tentativas da outbox |
| `retryDelaySeconds`, `retryDelayMs` | Espera solicitada para a próxima tentativa, conforme o worker |

Os logs registram tipos, códigos e identificadores técnicos, sem mensagens brutas de exceção, tokens, receipts SQS, connection strings ou payloads financeiros. Para erros sem tipo conhecido, use `stage`, `errorType`, os identificadores e o teste correspondente para localizar a origem no código.

## HTTP

`request_failed` identifica falhas não classificadas, com método, rota cadastrada, etapa e contexto disponível. `request_unavailable` identifica o caminho de indisponibilidade; `request_conflict` registra conflitos de identidade. A rota é o padrão do handler, sem os parâmetros de consulta recebidos.

`wager_failed` registra a primeira falha permanente auditada, com código SQL e identidade financeira. O HTTP devolve o resultado `FAILED`; um replay usa o mesmo resultado, sem nova tentativa financeira.

`42P01`, `42703` e `42883` indicam schema/deploy incompleto: geram indisponibilidade e rollback, sem consumir a identidade em `FAILED`. Corrija as migrations e repita a mesma chave e payload. No SQS, confira a DLQ se o reparo exceder o número de recebimentos permitido.

As etapas indicam a operação: `open_wallet`, `read_wallet`, `process_wager`, `read_transaction`, `read_ledger` ou `reconcile_wallet`. Falhas técnicas não expõem detalhes na resposta ao cliente.

## Falha terminal FAILED

`FAILED` é uma falha técnica auditada, distinta de `REJECTED` por regra de negócio. Corrigir a causa ou reenviar a mensagem original não reabre o resultado terminal.

1. Localize a transação e a causa por `transactionId`, correlação e SQLSTATE. Confira saldo, ledger, inbox e outbox e execute a reconciliação.
2. Corrija a causa de implementação ou infraestrutura, preservando o terminal, seu hash e seu código de falha. Não altere status ou saldo por SQL para forçar replay.
3. Se a mensagem chegou à DLQ, mantenha o vínculo com a investigação: reenviar a mesma identidade continuará retornando `FAILED`.
4. Se o negócio exigir uma nova movimentação, ela precisa ser uma nova operação, com novos ID externo e chave, rastreada ao incidente e definida após a conferência financeira. Não existe recuperação automática de `FAILED` neste projeto.

## Consumidor SQS

| Evento e etapa | Interpretação |
| --- | --- |
| `sqs_message_retry`, `decode` | JSON, envelope ou campos canônicos inválidos; nenhum processamento financeiro ocorreu nessa tentativa |
| `sqs_message_retry`, `process` | Falha no caso de uso; a categoria distingue conflito, validação, banco e prazo |
| `sqs_message_retry`, `acknowledge` | O caso de uso confirmou, mas `DeleteMessage` falhou; a reentrega deve retornar replay |
| `sqs_visibility_failed`, `change_visibility` | Não foi possível ajustar a visibilidade; a reentrega passa a depender do prazo já mantido pelo broker |
| `sqs_receive_failed`, `receive` | Falha ao buscar o lote; ainda não há mensagem individual associada |

`sqs_processed` confirma que o caso de uso retornou após o commit e inclui transação, status e indicação de replay. `sqs_completed` também confirma a remoção bem-sucedida da mensagem. Uma pendência durável pode ter ambos os eventos: sua continuação fica sob responsabilidade do worker de referências.

A ausência de `sqs_completed` após `sqs_processed` exige conferir a confirmação no broker, antes de concluir que a movimentação falhou. Rejeições de negócio não são falhas técnicas de retry. Um `sqs_processed` com status `FAILED` significa auditoria confirmada, e não sucesso financeiro ou confirmação no broker. Mensagens inválidas, conflitantes ou com resultado `FAILED` permanecem sujeitas à DLQ.

## Outbox e referências

`outbox_publish_retry` distingue `decode_event`, `publish` e `confirm_publication`. `outbox_sent` confirma o envio ao broker, mas a confirmação local ainda pode falhar. Nesse caso, a nova tentativa preserva o mesmo `eventId` e snapshot.

`outbox_retry_schedule_failed` e `reference_retry_schedule_failed` indicam falha ao persistir a próxima tentativa; o lease continua sendo o mecanismo de retomada. Reservas perdidas são registradas em DEBUG como `outbox_lease_lost`, `outbox_retry_lease_lost` ou `reference_retry_lease_lost`. O worker antigo não confirma nem reagenda um lease vencido, liberado ou reassumido.

`reference_retry`, na etapa `resume_reference`, informa falhas técnicas na retomada de uma transação pendente. `reference_failed` indica que uma falha permanente foi auditada e encerrou a referência em `FAILED`. A resolução ou rejeição da referência deve ser conferida na consulta da transação. Falhas nas consultas de métricas geram `outbox_stats_failed` ou `dlq_stats_failed`; o último valor da métrica pode estar desatualizado.

A DLQ é consultada inicialmente e depois com pausa de `DLQ_POLL_INTERVAL` (30s por padrão), em um ciclo independente do backlog. A métrica é aproximada e pode refletir a amostra anterior durante esse intervalo; a consulta de prontidão às filas permanece independente.

Para backlog, compare `rate(wager_outbox_published_total[1m])` com a taxa de eventos produzidos, quantidade pendente e idade do mais antigo. Use os histogramas `wager_outbox_claim_duration_seconds`, `wager_outbox_send_duration_seconds` e `wager_outbox_confirm_duration_seconds` para separar custo de reserva, envio e confirmação. As amostras incluem tentativas malsucedidas. `wager_database_pool_acquired_connections`, `wager_database_pool_max_connections` e `wager_database_pool_empty_acquire_wait_seconds_total` ajudam a identificar saturação. São amostradas pelo publisher, com pausa de um segundo; avalie cada processo e não some os gauges globais de pendências entre réplicas.

A idade usa `clock_timestamp()` e `occurred_at` do próprio PostgreSQL. Comparar o timestamp persistido com o relógio do processo pode distorcer o atraso quando host e VM estão dessincronizados. Outbox vazia tem idade zero.

## Consultar os registros da operação

Use o provedor e o ID externo registrados nos logs:

```sh
docker compose -f docker-compose.local.yml exec -T postgres \
  psql -U jungle -d jungle -v provider_id=provider-a -v external_id=bet-001 <<'SQL'
SELECT id, wallet_id, status, failure_code, updated_at
FROM wager_transactions
WHERE provider_id = :'provider_id' AND external_transaction_id = :'external_id';

SELECT i.message_id, i.received_at, i.completed_at, i.transaction_id
FROM inbox i
JOIN wager_transactions t ON t.id = i.transaction_id
WHERE t.provider_id = :'provider_id' AND t.external_transaction_id = :'external_id';

SELECT o.event_id, o.event_type, o.attempts, o.next_attempt_at, o.lease_until, o.published_at
FROM outbox o
JOIN wager_transactions t ON o.payload->'data'->>'transactionId' = t.id::text
WHERE t.provider_id = :'provider_id' AND t.external_transaction_id = :'external_id'
ORDER BY o.sequence;
SQL
```

Transação terminal com inbox concluída e outbox pendente indica que o financeiro confirmou e a publicação ainda precisa avançar. `lease_until` vencido permite a retomada por outro publisher. Não altere estado, saldo ou histórico para forçar a recuperação; primeiro identifique o worker e a etapa que estão falhando.

Para conferir o saldo contra o ledger, use `POST /wallets/{walletId}/reconciliation` com token interno, conforme o [contrato da API](API.md). A reconciliação informa divergências sem reparar dados.
