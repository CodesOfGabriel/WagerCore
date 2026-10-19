# WagerCore

O WagerCore processa operações financeiras de jogos em Go, por HTTP e SQS FIFO. Carteira, transação, ledger e eventos são confirmados em uma transação PostgreSQL. A deduplicação permanece válida entre processos e após reinícios.

Implementa `BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`, abertura interna, referências pendentes, inbox, outbox e reconciliação. A API usa tokens do Keycloak e separa o serviço interno dos provedores.

- [Contratos da API e exemplos](docs/API.md)
- [Arquitetura e regras financeiras](ARCHITECTURE.md)
- [Execução e organização dos testes](docs/TESTES.md)
- [Teste de carga e resultados](docs/BENCHMARK.md)

## Ambiente local

Execute os comandos na raiz do repositório. O ambiente em containers exige Docker com Compose v2 e as portas 8080, 8081, 4566 e 5432 disponíveis. Para executar Go ou os testes no host, use Go 1.27.2. O race detector também exige CGO habilitado e um compilador C. Os exemplos de token usam `curl` e Python 3.

```sh
make up
```

`make up` mantém os logs no terminal. Para executar em segundo plano e aguardar os health checks:

```sh
docker compose -f docker-compose.local.yml up -d --build --wait
docker compose -f docker-compose.local.yml ps
```

`docker-compose.local.yml` inicia PostgreSQL, Keycloak e LocalStack, importa o realm, cria as três filas e aplica as migrations antes da API. Volumes preservam PostgreSQL e Keycloak entre execuções. As credenciais versionadas são exemplos locais.

| Serviço | Endereço |
| --- | --- |
| API | http://localhost:8080 |
| Keycloak | http://localhost:8081 |
| LocalStack | http://localhost:4566 |
| PostgreSQL | localhost:5432 |

```sh
curl -i http://localhost:8080/health/live
curl -i http://localhost:8080/health/ready
```

Liveness verifica o processo. Readiness consulta PostgreSQL e as três filas SQS. Ambos são públicos; as rotas de negócio e `/metrics` exigem autenticação.

Para encerrar preservando PostgreSQL e Keycloak:

```sh
make down
```

As filas locais são recriadas quando o LocalStack inicia. A outbox permanece no PostgreSQL; eventos já confirmados como publicados não são republicados automaticamente quando o emulador é recriado.

## Primeira operação

O realm `wagercore` contém `internal-service`, `provider-a` e `provider-b`, provisionados com client credentials. A audience da API é `wagercore-api`.

```sh
INTERNAL_TOKEN=$(./scripts/token.sh internal-service)
PROVIDER_TOKEN=$(./scripts/token.sh provider-a)

curl --fail-with-body http://localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"100.00","currency":"BRL"}}'
```

Copie o `id` retornado para `WALLET_ID`:

```sh
WALLET_ID='id-retornado-pela-abertura'

curl --fail-with-body http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:bet-001' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"bet-001\",\"playerId\":\"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-1\",\"gameId\":\"game-1\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"

curl --fail-with-body -X POST "http://localhost:8080/wallets/$WALLET_ID/reconciliation" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"
```

A aposta retorna saldo `75.00`. Repetir o mesmo envio devolve o resultado persistido com `idempotentReplay: true`. Repetir a abertura do mesmo jogador e moeda retorna `409`.

## Executar Go no host

```sh
docker compose -f docker-compose.local.yml stop app
docker compose -f docker-compose.local.yml up -d --wait postgres keycloak localstack
test -f .env || cp .env.example .env
set -a
. ./.env
set +a
make migrate-up
make run
```

A aplicação lê o ambiente do processo; o shell carrega `.env` no exemplo acima. O primeiro comando encerra a API do Compose e seus workers, liberando a porta 8080 e evitando consumidores concorrentes durante o debug. O arquivo `.env` existente é preservado. Confira seus endereços antes de aplicar migrations.

Para voltar à execução em containers, encerre `make run` com `Ctrl+C` e execute `make up`.

A configuração completa está em [.env.example](.env.example). `DATABASE_MAX_CONNECTIONS` limita o pool de cada processo. `WORKER_ROLES` aceita `all`, `none`, `consumer`, `publisher` ou `references`. `WORKER_CONCURRENCY` limita os lotes e o trabalho simultâneo de cada worker; o consumidor recebe no máximo dez mensagens por lote.

`REFERENCE_TTL`, `REFERENCE_MAX_ATTEMPTS` e `RETRY_BASE` controlam referências ausentes. `LEASE_DURATION` controla reservas de publicação e retomada; `SQS_VISIBILITY_TIMEOUT` controla a reentrega do broker. Os padrões dão margem aos prazos de operação, de cinco segundos nos publishers e dez no consumidor.

Em produção, os workers ativos exigem janelas maiores que a tentativa completa: lease acima de 12s para publisher/referências (5s de reserva + 5s de processamento + 2s de limpeza) e visibilidade acima de 12s para o consumidor (10s + 2s). O padrão continua em 30s. `local` e `test` permitem os intervalos curtos usados nos cenários de recuperação.

`OIDC_ISSUER_URL` precisa corresponder ao emissor do token. `OIDC_JWKS_URL` pode usar o endereço interno do Keycloak; isso permite validar no container tokens obtidos pelo host. Para usar AWS, deixe `SQS_ENDPOINT`, `AWS_ACCESS_KEY_ID` e `AWS_SECRET_ACCESS_KEY` vazios e forneça credenciais pela cadeia padrão do SDK, com acesso às filas indicadas na arquitetura.

Com `APP_ENV=production`, os defaults locais de OIDC, SQS e credenciais são removidos. Configure explicitamente `OIDC_ISSUER_URL`, `OIDC_JWKS_URL` e `OIDC_AUDIENCE`; os dois URLs exigem HTTPS e não aceitam loopback nem aliases locais dos emuladores. Deixe `SQS_ENDPOINT` ausente ou vazio para usar o endpoint AWS da região; um endpoint explícito também exige HTTPS e host não local. Credenciais `test` são recusadas. O SDK usa a cadeia padrão da AWS, preservando `AWS_SESSION_TOKEN` e credenciais de roles. Mudar apenas `APP_ENV` no `.env.example` faz a inicialização falhar.

No publisher, `WORKER_INTERVAL` é a pausa dos ciclos sem trabalho ou com falha na reserva. Lotes disponíveis são publicados em sequência, mantendo o limite de concorrência e a ordem por carteira. Outbox e pool são amostrados com pausa de 1s; a DLQ tem ciclo independente, controlado por `DLQ_POLL_INTERVAL` (padrão 30s, faixa de 1s a 1h).

`SIGINT` e `SIGTERM` encerram o HTTP, cancelam os workers e fecham as conexões. Configuração inválida ou dependências indisponíveis impedem a inicialização.

## Migrations

```sh
make migrate-up
make migrate-status
make migrate-down
```

`up` aplica todas as pendentes em uma transação com lock de migração. `down` reverte uma versão. O histórico verifica checksum; arquivos já aplicados não devem ser alterados.

| Versão | Conteúdo |
| --- | --- |
| 1 | Carteiras, transações, ledger e invariantes financeiras |
| 2 | Retenção de transações auditáveis |
| 3 | Inbox, snapshots da outbox, ordem por carteira e reservas |
| 4 | Rastreio da origem e rejeições por limites de saldo e versão |

A reversão da versão 4 falha se houver rejeições incompatíveis com a restrição antiga. Reverter a versão 3 remove o histórico de mensageria; reverter a versão 1 remove os dados financeiros. Reversões não fazem parte da inicialização normal.

## Testes

Verificações sem dependências externas:

```sh
make check
make build
```

`make check` executa testes sem a tag de integração, race detector e `go vet`. `make build` gera `bin/server` e `bin/migrate`. Esses comandos não substituem os testes financeiros com serviços reais.

Integração completa:

```sh
make test-integration
```

`scripts/test-integration.sh` prepara PostgreSQL em 55432, Keycloak em 58081 e LocalStack em 54566 e executa os testes. O script remove os containers após sucesso e os preserva, com logs, em caso de falha ou interrupção. Depois da investigação, `make test-deps-down` encerra o ambiente.

`make test-concurrency` e `make test-failover` usam o mesmo script. Os testes iniciam processos Go próprios e usam serviços separados do ambiente de desenvolvimento.

`make test-failover` seleciona interrupções, indisponibilidade das dependências, perda de lease, falhas de schema/permanentes e shutdown em andamento. A integração completa também executa uma carga sustentada curta, com drenagem da outbox e conferência dos snapshots no SQS. `make benchmark` compara carga entre uma e três instâncias e grava `.local/benchmark.json`; `make benchmark-e2e` distribui 200 requisições a 20/s e grava `.local/benchmark-e2e.json`. Ambos exigem publicação integral dentro do prazo, além de integridade financeira.

A [documentação dos testes](docs/TESTES.md) detalha os alvos, isolamento, execução de um cenário e diagnóstico.

Material adicional: [cenários de validação](docs/CENARIOS.md), [diagnóstico operacional](docs/DIAGNOSTICO.md) e [registro das auditorias](docs/AUDITORIA.md).

## Estrutura

```text
cmd/server/                  Entrada da aplicação
cmd/migrate/                 Migrações versionadas
internal/domain/             Money, entidades e eventos imutáveis
internal/application/        Casos de uso e portas transacionais
internal/adapters/           HTTP, PostgreSQL, OIDC e SQS
internal/platform/           Configuração, workers, métricas e falhas de teste
internal/bootstrap/          Composição e ciclo de vida com Fx
migrations/                  Schema e restrições
tests/integration/           Processos, serviços reais e recuperação
deployments/                 Realm e filas locais
```
