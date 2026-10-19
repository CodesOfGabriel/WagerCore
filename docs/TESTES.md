# Testes

Os comandos abaixo devem ser executados na raiz do repositório, com Go 1.27.2. O race detector exige CGO habilitado e compilador C disponível. A integração também exige Docker com Compose v2 e as portas 55432, 58081 e 54566 livres.

## Verificações sem infraestrutura externa

```sh
make check
make build
```

| Alvo | Comando executado | Escopo |
| --- | --- | --- |
| `make test` | `go test ./...` | Domínio, configuração, JWT, HTTP, composição Fx e diagnóstico dos workers |
| `make race` | `go test -race ./...` | Mesma suíte com detecção de acessos concorrentes à memória |
| `make vet` | `go vet ./...` | Análise estática |
| `make check` | Os três alvos anteriores | Verificação conjunta |
| `make build` | Build de `cmd/server` e `cmd/migrate` | Binários em `bin/` |

Essa suíte usa servidores HTTP temporários quando precisa verificar JWT, handlers e erros do SDK. Não exige PostgreSQL, Keycloak ou LocalStack. A tag `integration` fica excluída; portanto, passar em `make check` não comprova o fluxo financeiro com banco e filas reais.

Para repetir a suíte sem aproveitar o cache de resultados:

```sh
go test -race -count=1 ./...
```

## Integração com serviços reais

```sh
make test-deps-up
make test-integration
make test-deps-down
```

A preparação usa `docker-compose.test.yml`, no projeto Compose `jungle-tests`, e aguarda os health checks. Ela inicia somente as dependências. `make test-integration` executa:

```sh
go test -race -tags=integration ./tests/integration/... -count=1 -timeout=5m
```

O Makefile exporta os endereços de teste. A suíte usa pgx contra PostgreSQL, obtém tokens por client credentials no Keycloak e envia e recebe mensagens pelo SDK do SQS no LocalStack. Os cenários HTTP iniciam processos Go a partir do próprio binário de testes, em portas atribuídas pelo sistema. Não é necessário executar `make run` ou iniciar a API no Compose local.

| Recurso | Isolamento e limpeza |
| --- | --- |
| PostgreSQL | Cada teste cria um schema aleatório, aplica as migrations necessárias e remove o schema ao terminar |
| Keycloak | Realm e clients importados do arquivo versionado; porta e container separados do ambiente local |
| SQS | Filas compartilhadas entre os cenários da suíte; os testes de mensageria consomem e removem mensagens durante a preparação e a execução |
| Processos Go | Cada cenário encerra seus processos ao terminar; os testes de queda observam a saída forçada e iniciam outro processo |

O usuário PostgreSQL de teste precisa criar e remover schemas. Não aponte essa suíte para banco ou filas usados pela aplicação: há testes de DDL, alteração deliberada de dados e limpeza de mensagens. Não execute duas suítes de integração simultaneamente no mesmo ambiente de teste.

O PostgreSQL de teste usa armazenamento temporário. `make test-deps-down` remove os containers; a próxima preparação começa com um banco vazio. Os alvos antigos `test-db-up` e `test-db-down` são aliases operacionais da mesma preparação e encerramento dos três serviços.

## Executar grupos ou um cenário

Com as dependências iniciadas:

```sh
make test-concurrency
make test-failover
```

| Alvo | Seleção |
| --- | --- |
| `test-concurrency` | `TestThreeProcessesConcurrentBets` e `TestSQSInboxAndCrossChannelIdempotency` |
| `test-failover` | Testes com prefixos `Crash`, `ReferenceWorker`, `MessagingRetention` e `AtomicRollback` |

Esses alvos também usam race detector e `-count=1`, com limite de três minutos. São recortes da suíte completa, não verificações adicionais obrigatórias após `make test-integration`.

Para executar um teste específico, com saída detalhada:

```sh
TEST_DATABASE_URL='postgres://jungle_test:jungle_test@localhost:55432/jungle_test?sslmode=disable' \
TEST_OIDC_ISSUER_URL='http://localhost:58081/realms/wagercore' \
TEST_SQS_ENDPOINT='http://localhost:54566' \
  go test -race -tags=integration ./tests/integration \
  -run '^TestSQSInboxAndCrossChannelIdempotency$' -count=1 -v -timeout=3m
```

O comando direto precisa de `TEST_DATABASE_URL`; a ausência causa falha explícita. OIDC e SQS usam os endereços de teste acima como fallback. Ao mudar portas, atualize a composição e as variáveis correspondentes.

Para explorar parsing e aritmética de valores monetários:

```sh
go test ./internal/domain -run='^$' -fuzz=FuzzMoneyParsing -fuzztime=10s
go test ./internal/domain -run='^$' -fuzz=FuzzMoneyArithmetic -fuzztime=10s
```

## Como interpretar os resultados

Os testes conferem o estado confirmado no banco, além da resposta HTTP. A [matriz de cenários](CENARIOS.md) relaciona cada teste às invariantes verificadas.

Nos cenários de concorrência, três processos independentes acessam o mesmo schema. Os testes exigem um único débito para 50 envios iguais e uma única aposta aprovada quando duas apostas de 80 disputam saldo 100.

Os cenários de recuperação usam prazos menores que os padrões da aplicação. Os pontos de falha encerram um processo com código 97 após o commit financeiro, após o commit da inbox ou após enviar o evento. Esse encerramento é esperado pelo teste; a aprovação depende da recuperação por outro processo, sem duplicação financeira.

O cenário de falha na confirmação SQS usa um proxy que rejeita apenas `DeleteMessage`. PostgreSQL, Keycloak e as demais chamadas ao LocalStack continuam reais. O teste exige log na etapa `acknowledge`, financeiro já confirmado e replay após liberar a confirmação.

No SQS, os testes usam IDs de deduplicação de transporte distintos em alguns reenvios. Assim, a correção precisa vir da inbox e da identidade financeira, sem depender apenas da janela de deduplicação FIFO. Mensagens inválidas precisam chegar à DLQ após a política de recebimentos.

Uma falha encerra o comando com código diferente de zero. Antes de repetir, consulte o nome do teste e sua saída. Quando há falha nos helpers HTTP ou na inicialização, a saída inclui os logs capturados do processo. Para falhas nas dependências:

```sh
docker compose -p jungle-tests -f docker-compose.test.yml ps
docker compose -p jungle-tests -f docker-compose.test.yml logs --tail=100 postgres-test keycloak-test localstack-test
```

`connection refused` indica endereço inacessível ou serviço ainda não iniciado. Falha em client credentials exige conferir importação do realm, client e secret. Um timeout de cenário exige examinar o estado do processo e as etapas de retry; não aumente o prazo antes de identificar o bloqueio. O [guia de diagnóstico](DIAGNOSTICO.md) descreve os campos dos logs.

## Integração contínua

O workflow `.github/workflows/test.yml` possui dois jobs. O primeiro verifica formatação, dependências, testes, race detector, análise estática e build. O segundo prepara os três serviços e executa a suíte completa de integração. Em caso de falha, publica os logs dos serviços na execução; o encerramento das dependências ocorre mesmo quando os testes falham.

A CI valida processos no mesmo host com SQS emulado. Não representa um ensaio de indisponibilidade regional da AWS ou de um cluster PostgreSQL.
