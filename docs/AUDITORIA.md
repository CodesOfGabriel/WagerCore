# Verificação da auditoria técnica

Revisão de 19/10/2026 sobre a base `d13e6e6`, com correções locais posteriores. A comparação usa o código, os testes reais e a execução do Compose. Os resultados antigos do benchmark foram preservados e identificados como anteriores à correção.

## Pontos verificados

| Ponto da auditoria | Constatação e alteração | Verificação |
| --- | --- | --- |
| 2.1 — Gargalo da outbox | O publisher aguardava 200 ms e consultava a DLQ após cada lote, mesmo ocupado. Agora reserva o próximo lote imediatamente; as estatísticas têm ciclo próprio. Concorrência quatro, pool dez e ordem por carteira foram preservados. | Carga em um e três processos; histogramas de reserva, envio e confirmação; métricas de saturação do pool. |
| 2.2 — Retry sem conferir linhas | Os dois métodos podiam retornar sucesso com zero alterações. Agora exigem token, lease válido e exatamente uma linha alterada; retornam `ErrLeaseLost` nos demais casos. | `TestRetryLeaseFencing` e `TestMessagingRetentionOrderingAndLeaseFencing`, com PostgreSQL real. |
| 2.3 — Benchmark sem drenagem | O benchmark anterior encerrava com eventos pendentes. Agora aguarda publicação integral e confere IDs, bytes e ordem no SQS antes da limpeza. | `TestOutboxSustainedLoad` na integração/CI; `make benchmark` e `make benchmark-e2e`, com relatório de produção, publicação e recuperação. |
| 2.4 — Defaults locais em produção | `APP_ENV=production` remove os defaults de OIDC/SQS/credenciais, exige identidade explícita por HTTPS e recusa hosts locais e credenciais `test`. Produção usa a cadeia AWS, incluindo sessão e roles. | Testes de configuração e `TestProductionPreservesAWSSessionCredentials`. |
| 2.5 — Readiness depende de SQS | Comportamento mantido conforme o contrato. Documentado que o balanceador pode retirar a API mesmo quando o PostgreSQL ainda permitiria confirmar a operação e sua outbox. | `TestSQSOutageRetainsCommittedEvents` observa readiness `503`, BET direta confirmada e dois eventos pendentes que depois são publicados; justificativa em `ARCHITECTURE.md`. |
| 2.6 — Política de `FAILED` | `42P01`, `42703` e `42883` passam a indisponibilidade recuperável. Violações `23514`, `23503`, `22003` e `22P02` continuam terminais por decisão explícita de integridade. | `TestDatabaseFailureClassification`, `TestSchemaFailureCanBeRetriedAfterRepair` por HTTP/SQS e testes de falha permanente, inclusive no commit e em referências. |

As garantias financeiras, autenticação, idempotência, snapshots imutáveis, coordenação por carteira, inbox e recuperação continuam cobertas pelos cenários de [CENARIOS.md](CENARIOS.md). Nenhuma migration aplicada foi alterada.

Ao conferir o primeiro relatório sustentado, foi identificado outro problema de medição: a idade comparava `occurred_at` do banco com o relógio do host e podia ficar negativa. O benchmark e `OutboxStats` passaram a usar o horário do próprio PostgreSQL, lido na mesma consulta. `TestOutboxStatsUseDatabaseClock` verifica a faixa de idade contra duas leituras do banco e o caso de outbox vazia. Os benchmarks foram repetidos após essa correção.

## CI do commit auditado

Os jobs `unit` e `integration` do [workflow 39285992420](https://github.com/CodesOfGabriel/WagerCore/actions/runs/39285992420) foram consultados e estavam concluídos com `success`. A observação anterior de integração ainda em execução ficou desatualizada. Esse resultado corresponde ao commit auditado; as correções locais ainda precisam de uma execução remota após publicação.

## Inicialização sem estado anterior

Foi exportada uma cópia dos fontes versionáveis para `.local/audit-clean`, sem `.env`, cache, binários anteriores ou volumes existentes. O mesmo `docker-compose.local.yml` subiu no projeto separado `wagercore-audit-clean`, com um override apenas de portas e issuer: API 18080, Keycloak 18081, SQS 14566 e PostgreSQL 15432. O ambiente de desenvolvimento existente foi preservado.

O build terminou; a migração retornou `version=4 changed=4`; PostgreSQL, Keycloak, LocalStack e API ficaram saudáveis. O fluxo HTTP abriu uma carteira com `100.00`, processou BET de `25.00`, devolveu replay da mesma transação com saldo `75.00` e reconciliou dois lançamentos com diferença zero. Acesso sem token retornou `401`; outro provedor não leu a transação (`404`). No banco, os quatro eventos estavam publicados e não havia pendências. O projeto temporário e seus volumes foram removidos depois da conferência.

Os logs e o JSON da execução ficam em `.local/audit-clean-startup.log` e `.local/audit-smoke.json`, fora do versionamento. Foi uma instalação local isolada dos fontes em revisão; não representa validação em AWS real.

## Comandos de validação

`make check build` passou: testes sem infraestrutura, race detector, `go vet` e os dois binários. `make test-integration` passou em 229,999 segundos, com race detector e sem cache de resultados, incluindo a carga sustentada curta e os novos testes de regressão. O recorte `make test-failover` também passou, em 114,305 segundos. `make benchmark` passou: 600 eventos recebidos por perfil, sem erro ou divergência.

Depois do ajuste da medição de idade, `make check build` foi repetido com sucesso. A regressão do relógio e os três perfis de rajada também passaram, em 92,141 segundos de suíte: a recuperação final foi de 3,41 a 9,50 segundos. A execução anterior mediu 1,98 a 4,52 segundos; a variação está registrada e não foi tratada como prova de ganho de capacidade.

`make benchmark-e2e` passou e foi repetido após o ajuste de idade, usando os mesmos parâmetros e dependências já preparadas. A repetição final passou em 71,538 segundos: 400 eventos recebidos por perfil, sem erros ou divergências, idades não negativas e recuperação entre 0,09 e 0,21 segundo. As repetições finais usaram `go test -race -tags=integration`, `-count=1`, filtro de `TestFinancialLoad`/regressão do relógio e os parâmetros dos respectivos alvos Make. As dependências foram encerradas ao terminar.

```sh
make check build
make test-integration
make test-failover
make benchmark
make benchmark-e2e
```

Os dois benchmarks não impõem uma taxa arbitrária de requisições. Exigem integridade, recuperação dentro do limite configurado e recebimento dos snapshots esperados. Os resultados e limites de interpretação estão em [BENCHMARK.md](BENCHMARK.md).
