# Testes

A suíte sem infraestrutura verifica regras locais; a integração usa PostgreSQL, Keycloak e SQS reais. Execute os comandos na raiz do repositório, com Go 1.27.2. O race detector exige CGO habilitado e compilador C disponível. A integração também exige Docker com Compose v2 e as portas 55432, 58081 e 54566 livres.

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
| `make vuln` | `govulncheck ./...` (v1.9.0) | Vulnerabilidades alcançáveis; consulta a base pública Go e exige rede |
| `make check` | Os três alvos anteriores | Verificação conjunta |
| `make build` | Build de `cmd/server` e `cmd/migrate` | Binários em `bin/` |

Servidores HTTP temporários verificam JWT, handlers e erros do SDK. Esses testes não exigem PostgreSQL, Keycloak ou LocalStack. A tag `integration` fica excluída; portanto, passar em `make check` não comprova o fluxo financeiro com banco e filas reais.

Para repetir a suíte sem aproveitar o cache de resultados:

```sh
go test -race -count=1 ./...
```

## Integração com serviços reais

```sh
make test-integration
```

`scripts/test-integration.sh` controla o ciclo de vida do ambiente. O script registra a limpeza antes de iniciar as dependências, sobe `docker-compose.test.yml` no projeto `jungle-tests` e aguarda os health checks. Depois, executa a suíte com race detector, tag `integration`, cache de resultados desabilitado e timeout de cinco minutos.

Após sucesso, o script remove os containers e a rede com `docker compose down`. Falhas e interrupções preservam o ambiente e exibem os últimos cem registros de cada serviço. Erro de preparação, teste ou limpeza mantém o status de falha.

O Makefile define os endereços de teste; o script fornece um endereço PostgreSQL padrão para execução direta. A suíte usa pgx contra PostgreSQL, obtém tokens por client credentials no Keycloak e envia e recebe mensagens pelo SDK do SQS no LocalStack. Os cenários HTTP iniciam processos Go a partir do próprio binário de testes, em portas atribuídas pelo sistema. Não é necessário executar `make run` ou iniciar a API no Compose local.

| Recurso | Isolamento e limpeza |
| --- | --- |
| PostgreSQL | Cada teste cria um schema aleatório, aplica as migrations necessárias e remove o schema ao terminar |
| Keycloak | Realm e clients importados do arquivo versionado; porta e container separados do ambiente local |
| SQS | Filas compartilhadas entre os cenários da suíte; os testes de mensageria consomem e removem mensagens durante a preparação e a execução |
| Processos Go | Cada cenário encerra seus processos ao terminar; os testes de queda observam a saída forçada e iniciam outro processo |

O usuário PostgreSQL de teste precisa criar e remover schemas. Não aponte essa suíte para banco ou filas usados pela aplicação: há testes de DDL, alteração deliberada de dados e limpeza de mensagens. Não execute duas suítes de integração simultaneamente no mesmo ambiente de teste.

O PostgreSQL de teste usa armazenamento temporário: remover os containers garante banco vazio na próxima execução. O encerramento automático não remove os volumes do ambiente local.

## Executar grupos ou um cenário

Os dois alvos abaixo usam a mesma preparação automática, limpeza após sucesso e preservação do ambiente em caso de falha:

```sh
make test-concurrency
make test-failover
```

| Alvo | Seleção |
| --- | --- |
| `test-concurrency` | `TestThreeProcessesConcurrentBets` e `TestSQSInboxAndCrossChannelIdempotency` |
| `test-failover` | Testes com prefixos `Crash`, `ReferenceWorker`, `MessagingRetention`, `RetryLease`, `AtomicRollback`, `PostgresOutage`, `SQSOutage`, `SchemaFailure`, `Permanent` e `Shutdown` |

Esses alvos também usam race detector e `-count=1`, com limite de três minutos. São recortes da suíte completa, não verificações adicionais obrigatórias após `make test-integration`.

Para executar um teste específico, passe os filtros ao mesmo script:

```sh
./scripts/test-integration.sh \
  -run '^TestSQSInboxAndCrossChannelIdempotency$' -v
```

O script aceita os argumentos adicionais de `go test`. `TEST_TIMEOUT` altera seu limite; os alvos de concorrência e recuperação usam três minutos. `TEST_DATABASE_URL`, `TEST_OIDC_ISSUER_URL` e `TEST_SQS_ENDPOINT` permitem ajustar os endereços, desde que correspondam ao ambiente de teste da composição.

A execução direta de `go test -tags=integration` não passa pelo script e não gerencia containers. `TEST_DATABASE_URL` é obrigatório nessa forma de execução. Para o uso normal, prefira os alvos Make ou o script acima.

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

Pausas controladas nos containers PostgreSQL e LocalStack verificam indisponibilidade real, preservando os dados temporários. Os testes sempre tentam desfazer a pausa antes de encerrar. Esses cenários exigem acesso ao mesmo Docker usado na preparação e não devem rodar em paralelo com outra suíte.

Nos testes de shutdown, uma operação no banco ou a resposta de publicação no broker fica bloqueada antes do SIGTERM. Os testes verificam conclusão da requisição HTTP, rollback da mensagem interrompida, liberação de visibilidade e retomada da publicação com o mesmo evento.

No SQS, os testes usam IDs de deduplicação de transporte distintos em alguns reenvios. Assim, a correção precisa vir da inbox e da identidade financeira, sem depender apenas da janela de deduplicação FIFO. Mensagens inválidas precisam chegar à DLQ após a política de recebimentos.

Uma falha encerra o comando com código diferente de zero. Quando os helpers HTTP ou a inicialização falham, a saída inclui os logs capturados dos processos. O script acrescenta os logs das dependências e mantém seus containers ativos. Consulto a saída do teste para os logs dos processos Go e uso os comandos abaixo para continuar a investigação dos serviços:

```sh
docker compose -p jungle-tests -f docker-compose.test.yml ps
docker compose -p jungle-tests -f docker-compose.test.yml logs -f --tail=100
```

Depois da investigação, execute `make test-deps-down` para liberar os recursos. Preservar os containers não altera a limpeza dos schemas nem o encerramento dos processos Go realizados pelos próprios testes; os dados de cada schema continuam sendo removidos ao final do cenário.

Os comandos abaixo permitem inspecionar as dependências fora de uma execução de testes:

```sh
make test-deps-up
docker compose -p jungle-tests -f docker-compose.test.yml ps
docker compose -p jungle-tests -f docker-compose.test.yml logs --tail=100 postgres-test keycloak-test localstack-test
make test-deps-down
```

`test-db-up` e `test-db-down` executam a mesma preparação e encerramento dos três serviços. Essa inspeção manual exige o encerramento explícito. Se o script for encerrado com SIGKILL ou o Docker não responder durante a limpeza, execute `make test-deps-down` quando o daemon estiver disponível.

`connection refused` indica endereço inacessível ou serviço ainda não iniciado. Falha em client credentials exige conferir importação do realm, client e secret. Um timeout de cenário exige examinar o estado do processo e as etapas de retry; não aumente o prazo antes de identificar o bloqueio. O [guia de diagnóstico](DIAGNOSTICO.md) descreve os campos dos logs.

## Teste de carga

`TestOutboxSustainedLoad` executa na integração normal e na CI: 60 requisições a 20/s em cada um dos três perfis, com até trinta segundos para drenar a outbox. Além de saldo, versão e ledger, confere todos os IDs, snapshots e ordem por carteira recebidos no SQS.

`TestFinancialLoad` fica desabilitado na integração normal. `make benchmark` habilita esse cenário com uma rajada de 300 requisições por perfil. `make benchmark-e2e` usa 200 requisições a 20/s. Ambos mantêm a mesma preparação e limpeza e exigem drenagem completa; o relatório separa throughput HTTP, publicação durante a carga e throughput até confirmar todos os eventos. A metodologia, os parâmetros e os resultados estão em [BENCHMARK.md](BENCHMARK.md).

## Integração contínua

`.github/workflows/test.yml` tem dois jobs: `unit` verifica formatação, dependências, vulnerabilidades alcançáveis (`make vuln`), testes, race detector, análise estática e build; `integration` executa `make test-integration`, com logs e limpeza controlados pelo mesmo script local. O scanner tem versão fixada no Makefile; a base de vulnerabilidades é atualizada pelo projeto Go.

A CI valida processos no mesmo host com SQS emulado. Não representa um ensaio de indisponibilidade regional da AWS ou de um cluster PostgreSQL.
