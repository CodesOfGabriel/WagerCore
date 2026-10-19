# WagerCore

Backend em Go para processamento distribuído de operações financeiras de jogos.

O projeto contém a base HTTP com Uber Fx, o domínio financeiro e migrations de carteiras, transações e ledger. Os endpoints financeiros serão implementados com autenticação na próxima etapa.

- [Decisões de arquitetura](ARCHITECTURE.md)
- [Cenários de resiliência e desempenho](docs/CENARIOS.md)

## Pré-requisitos

- Go 1.27.2 para executar e testar fora dos containers.
- Docker Engine e Docker Compose v2 para o ambiente local.
- Portas 8080 e 5432 disponíveis.
- Porta 55432 disponível para os testes de integração.

## Executar com Docker

```sh
docker compose -f docker-compose.local.yml up --build
```

O Compose inicia PostgreSQL, aplica as migrations e inicia a aplicação. O banco utiliza um volume persistente e credenciais locais de exemplo. Uma falha nas migrations impede o início da aplicação. `make up` executa o mesmo comando.

```sh
curl -i http://localhost:8080/health/live
curl -i http://localhost:8080/health/ready
```

| Rota | Resposta |
| --- | --- |
| `GET /health/live` | `200` com `{"status":"alive"}` |
| `GET /health/ready` | `200` se o PostgreSQL responder; `503` se estiver indisponível |

Nesta etapa, readiness verifica apenas PostgreSQL. A verificação de SQS será incluída junto da mensageria. Os health checks são públicos. Os endpoints financeiros serão adicionados com autenticação OIDC.

Para encerrar preservando os dados:

```sh
make down
```

## Executar o Go localmente

Inicie apenas o banco:

```sh
docker compose -f docker-compose.local.yml up -d --wait postgres
cp .env.example .env
set -a
. ./.env
set +a
go run ./cmd/migrate up
go run ./cmd/server
```

A aplicação lê variáveis do processo. O carregamento do arquivo `.env` é feito pelo shell nos comandos acima.

| Variável | Padrão | Uso |
| --- | --- | --- |
| `DATABASE_URL` | Obrigatória | Conexão PostgreSQL |
| `HTTP_ADDR` | `:8080` | Endereço HTTP local |
| `HEALTH_TIMEOUT` | `2s` | Prazo de verificação do banco, maior que zero e até `5s` |
| `TEST_DATABASE_URL` | Definida no Makefile | Banco separado para os testes de integração |

No Compose, os endereços da aplicação e do banco estão definidos para a rede dos containers. `HEALTH_TIMEOUT` pode ser sobrescrito pelo ambiente ou por `.env`.

A inicialização falha quando a configuração é inválida, o banco está indisponível ou a porta HTTP está ocupada. `SIGINT` e `SIGTERM` iniciam o encerramento do servidor antes do fechamento do pool PostgreSQL.

## Migrations

Com o PostgreSQL local iniciado:

```sh
make migrate-up
make migrate-status
```

`migrate-up` aplica todas as versões pendentes. `migrate-status` informa a versão atual. Esses comandos usam `DATABASE_URL`, com o mesmo endereço local de `.env.example` como padrão.

Para reverter a última versão:

```sh
make migrate-down
```

Cada execução de `down` reverte uma versão. A versão 2 protege a retenção das transações. Reverter a versão 1 remove as tabelas financeiras e seus dados. As reversões são comandos explícitos e não fazem parte da inicialização normal.

Os arquivos SQL ficam em `migrations/` e são incluídos no binário `migrate`. O histórico guarda versão e checksum. Alterar uma migration já aplicada bloqueia a execução; mudanças no banco exigem uma nova versão.

## Testes

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o bin/server ./cmd/server
go build -o bin/migrate ./cmd/migrate
docker compose -f docker-compose.local.yml config --quiet
```

Os testes sem build tag verificam o domínio, a configuração, os handlers de saúde, a composição Fx e o ciclo de vida HTTP.

Para verificar migrations e restrições em PostgreSQL real:

```sh
make test-db-up
make test-integration
make test-db-down
```

O banco de testes usa a porta 55432 e armazenamento temporário, separado do banco local na porta 5432. Cada teste cria seu próprio schema e o remove ao terminar. A ausência de `TEST_DATABASE_URL` causa falha explícita quando a suíte é chamada diretamente:

```sh
TEST_DATABASE_URL='postgres://jungle_test:jungle_test@localhost:55432/jungle_test?sslmode=disable' \
  go test -race -tags=integration ./tests/integration/...
```

A suíte cobre aplicação e reversão das migrations, checksum, saldo não negativo, unicidade, imutabilidade do ledger, atomicidade, referências e limites de `int64`. Ainda não representa o processamento por HTTP/SQS nem os testes com três instâncias.

Para explorar parsing e aritmética com fuzzing:

```sh
go test ./internal/domain -run='^$' -fuzz=FuzzMoneyParsing -fuzztime=10s
go test ./internal/domain -run='^$' -fuzz=FuzzMoneyArithmetic -fuzztime=10s
```

O `Makefile` reúne os comandos de execução, verificação, migrations e banco de testes.

## Estrutura

```text
cmd/server/                 Entrada da aplicação
cmd/migrate/                Aplicação e reversão das migrations
internal/bootstrap/         Composição e ciclo de vida com Fx
internal/platform/config/   Leitura e validação do ambiente
internal/adapters/http/     Servidor e health checks
internal/adapters/postgres/ Pool de conexões
internal/domain/            Money, Wallet, WagerTransaction e WalletLedgerEntry
migrations/                 Schema financeiro e proteções do histórico
tests/integration/          Verificações com PostgreSQL real
docs/                       Cenários de resiliência e desempenho
```

## Próxima etapa

Implementar Keycloak, autorização, repositórios e o caso de uso transacional de abertura e BET. A API deve confirmar resultado, carteira, ledger e outbox no mesmo commit.

Ainda estão pendentes os endpoints financeiros, o fluxo completo de idempotência, SQS, inbox/outbox, workers, reconciliação, métricas e testes distribuídos. O schema já protege as identidades financeiras; o tratamento de replay e conflito será implementado no caso de uso.
