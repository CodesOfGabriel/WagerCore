# Cenários de validação

Montei a suíte com PostgreSQL, Keycloak e LocalStack reais para verificar o estado confirmado no banco e o comportamento das filas. Os processos Go compartilham banco e filas, com portas HTTP próprias. Os comandos e o isolamento estão em [TESTES.md](TESTES.md); este documento relaciona cada cenário à garantia que ele verifica.

Na execução local de 19/10/2026, `make check build`, `make test-integration` e `make test-failover` passaram. O Docker local iniciou saudável com schema na versão 4. Uma demonstração com abertura, BET e WIN por HTTP, nova BET por SQS e replays terminou com saldo `80.00`, quatro lançamentos reconciliados, inbox concluída e nenhuma pendência de publicação.

## Financeiro e autorização

| Teste de integração | Verificação |
| --- | --- |
| `TestAuthenticatedWalletAndIdempotency` | Tokens reais, expiração, audience, papéis, isolamento de provedores, abertura positiva/zero, replay original, conflitos, paginação e reconciliação |
| `TestFinancialOperationsAndReversals` | WIN, LOSS, REFUND, ROLLBACK, rejeições de referência, reversão única, saldo insuficiente na reversão e overflow |
| `TestReconciliationReportsDivergenceWithoutRepair` | Corrupção deliberada em schema isolado aparece na resposta e na métrica, sem ajuste automático |
| `TestAtomicRollbackWhenOutboxFails` | Uma falha SQL na outbox reverte abertura ou aposta por inteiro |
| `TestThreeProcessesConcurrentBets` | Três processos, 50 envios da mesma BET, duas apostas concorrentes de 80 sobre 100 e replay após reinício |
| `TestSQSInboxAndCrossChannelIdempotency` | Disputa HTTP/SQS, mensagens repetidas, inbox, conflito de hash e OPENING inválida na DLQ |
| `TestSQSLogsAcknowledgementFailureAfterCommit` | Falha de DeleteMessage identificada após o commit, com request ID, transação e replay na reentrega |
| `TestReferenceWorkerResumesAfterRestart` | Referência recebida depois, continuação persistente e rejeição por prazo/tentativas |

Os testes consultam saldo, versão, transações, ledger, inbox e outbox conforme o cenário. No caso de 50 envios iguais, exigem uma operação aplicada e 49 replays. No caso de duas apostas de 80, exigem uma processada, uma rejeitada e saldo 20. Uma carteira bloqueada por outra conexão não impede o avanço de outra carteira.

A suíte também mantém os testes de constraints, imutabilidade, limites de `int64`, referências, aplicação e reversão de migrations, rollback de DDL e checksum.

## Interrupções e retomada

`make test-failover` seleciona os testes abaixo e a falha SQL antes do commit. A aplicação só permite os pontos de encerramento forçado no ambiente de teste, filtrados pelo ID externo.

| Teste | Evidência exigida |
| --- | --- |
| `TestCrashAfterFinancialCommitReplaysOriginalResult` | Processo termina antes da resposta; outra instância devolve o saldo original após novos movimentos |
| `TestCrashAfterSQSCommitRedeliversWithoutMovement` | Processo termina antes de remover a mensagem; outro recebe com `receiveCount >= 2`, confirma replay e preserva um único débito |
| `TestCrashAfterEventPublicationUsesStableSnapshot` | Processo termina antes de confirmar a outbox; outro republica o mesmo ID e bytes, com nova tentativa persistida |
| `TestMessagingRetentionOrderingAndLeaseFencing` | Lease expirado é reassumido; proprietário antigo não confirma; eventos da mesma carteira não se ultrapassam |

A contagem de envio da outbox é observada nos logs e nas tentativas do banco. A fila FIFO pode suprimir a segunda entrega com o mesmo ID dentro de sua janela; esse comportamento não é tratado como prova de publicação exatamente uma vez. Os consumidores de eventos precisam deduplicar por `eventId`.

A restauração de referências é verificada pelo estado final e pelo único lançamento correspondente. A mensagem SQS só é removida depois do commit; pendências são continuadas pelo worker de referências.

## Limites das evidências

Esses cenários demonstram processos independentes no mesmo host e broker emulado. Não simulam perda de disco, cluster PostgreSQL, indisponibilidade regional da AWS ou todos os modos de falha de rede. A CI prepara o mesmo ambiente e executa a suíte com race detector; seu resultado remoto depende da execução no GitHub.

## Distribuição HTTP com Traefik

Mantive o Traefik como extensão de infraestrutura. Primeiro validei a integridade diretamente por HTTP e SQS entre processos, sem depender de afinidade de sessão.

Uma futura composição com proxy deve encaminhar HTTP para três réplicas sem `container_name` fixo e usar `/health/ready` para retirar uma instância indisponível. O cliente pode repetir a operação em qualquer réplica. O proxy não deve repetir POST automaticamente. SQS continua entregando diretamente aos consumidores.

O acesso ao Docker deve passar por socket proxy restrito, com dashboard e infraestrutura em redes adequadas. A demonstração deve observar três processos, interromper um, conferir o atendimento pelos outros e validar os replays após sua recuperação.

## Desempenho

Ainda não executei um benchmark para publicar resultados de desempenho. A medição deve começar depois da integridade, com ambiente, recursos, versões, duração e proporção das operações registrados.

| Carga equivalente | Objetivo |
| --- | --- |
| Uma instância e 100 carteiras | Referência de processamento |
| Três instâncias e 100 carteiras | Distribuição entre carteiras independentes |
| Três instâncias e uma carteira | Custo da contenção necessária |

Coletar throughput, erros, p50/p95/p99, tempo de aquisição de conexão e lock, conflitos, atraso da outbox e divergências. Verificar planos das consultas com `EXPLAIN (ANALYZE, BUFFERS)` antes de ajustar pools ou índices. O tamanho do pool é configurável e deve respeitar o orçamento total das instâncias e workers.

As métricas implementadas fornecem latência, resultados, retries, conflitos e pendências. Os IDs ficam nos logs para evitar cardinalidade ilimitada. Números de desempenho só devem ser publicados a partir de execução registrada.
