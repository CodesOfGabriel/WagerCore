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

## Indisponibilidade e encerramento

| Teste | Evidência exigida |
| --- | --- |
| `TestPostgresOutagePreservesIdempotency` | Banco pausado produz readiness indisponível e `503`; após o retorno, a operação é aplicada uma vez e os eventos são publicados |
| `TestSQSOutageRetainsCommittedEvents` | Falta de resposta do broker mantém eventos no banco; o publisher retoma sem outro movimento financeiro |
| `TestPermanentInfrastructureFailureIsAudited` | Erro SQL permanente gera `FAILED`, preserva saldo e ledger, mantém replay terminal e encaminha a mensagem à DLQ |
| `TestPermanentFailureAtCommitIsAudited` | Falha em constraint adiada reverte carteira, ledger e outbox antes de registrar `FAILED` |
| `TestPermanentReferenceFailureIsAudited` | Falha permanente na retomada encerra a referência em `FAILED`, sem crédito parcial |
| `TestShutdownCompletesInFlightHTTPRequest` | SIGTERM aguarda a requisição em andamento, que confirma e pode ser repetida após reinício |
| `TestShutdownReleasesInFlightSQSMessage` | SIGTERM cancela o processamento bloqueado e libera uma mensagem com visibilidade de trinta segundos para retomada imediata |
| `TestShutdownRecoversUnconfirmedPublication` | O broker recebe o evento, a resposta é interrompida pelo shutdown e outro publisher confirma o mesmo evento |

Usei pausas nos containers para interromper respostas reais do banco e do broker, preservando seus dados. Nos cenários de shutdown, também confiro nos logs a sequência de encerramento do HTTP, dos workers e do pool PostgreSQL.

## Limites das evidências

Esses cenários demonstram processos independentes no mesmo host e broker emulado. Não simulam perda de disco, cluster PostgreSQL, indisponibilidade regional da AWS ou todos os modos de falha de rede. A CI prepara o mesmo ambiente e executa a suíte com race detector; seu resultado remoto depende da execução no GitHub.

## Desempenho

Criei um teste de carga com a mesma quantidade de operações em uma instância com cem carteiras, três instâncias com cem carteiras e três instâncias disputando uma carteira. O comando, os parâmetros, as métricas e os resultados medidos estão em [BENCHMARK.md](BENCHMARK.md).
