# Testes de carga

Os benchmarks usam PostgreSQL e SQS compartilhados para comparar uma e três instâncias Go. Validam integridade financeira, publicação integral e recuperação do backlog.

```sh
make benchmark
make benchmark-e2e
```

`benchmark` envia uma rajada de 300 requisições por perfil e grava `.local/benchmark.json`. `benchmark-e2e` distribui 200 requisições a 20/s e grava `.local/benchmark-e2e.json`. O nome do alvo é mantido; a medição termina na publicação e não inclui consumo downstream.

## Metodologia e parâmetros

Os três perfis são uma instância com 100 carteiras, três instâncias com 100 carteiras e três instâncias disputando uma carteira. Cada BET tem identidade nova e valor `0.01 BRL`; as carteiras começam com `100000.00 BRL`.

Antes da medição, os testes abrem as carteiras, aguardam e consomem os eventos de abertura e capturam as métricas iniciais. Todos os processos usam pool dez, publisher de concorrência quatro e pausa de 200 ms somente quando não encontram trabalho ou a reserva falha. O binário usa race detector.

| Parâmetro | Padrão e limites |
| --- | --- |
| `BENCHMARK_REQUESTS` | 300 na rajada, 200 na carga sustentada; 1–1000 |
| `BENCHMARK_CONCURRENCY` | 20 clientes; 1–50 |
| `BENCHMARK_RATE` | Ausente na rajada, 20/s no sustentado; 1–1000/s quando definido |
| `BENCHMARK_DRAIN_TIMEOUT` | 60s para recuperação; 1s–2m |
| `BENCHMARK_REPORT` | Caminho do JSON; relativo a `tests/integration` |

```sh
BENCHMARK_REQUESTS=600 BENCHMARK_RATE=20 BENCHMARK_DRAIN_TIMEOUT=90s make benchmark-e2e
```

O relatório separa taxa e latência HTTP, produção/publicação de eventos durante a carga, recuperação, pendências, erros, divergências e métricas de lock, pool e publicação. `financialToPublishedSeconds` e `financialToPublishedRequestsPerSecond` medem do primeiro envio HTTP até confirmar todos os eventos como publicados na outbox. A conferência dos snapshots recebidos no SQS ocorre depois, fora desse cronômetro. Esses campos substituem `endToEndSeconds` e `endToEndRequestsPerSecond` dos relatórios antigos.

Qualquer erro HTTP, divergência de saldo/versão/ledger, atraso acima do limite ou snapshot ausente, alterado ou fora de ordem faz o teste falhar. Duplicatas idênticas são deduplicadas por `eventId`. Os containers são removidos após sucesso e preservados em falhas.

`TestOutboxSustainedLoad` executa a versão curta na integração e na CI: 60 requisições a 20/s por perfil, com até 30s para drenar a outbox.

## Resultados de referência — e5c023d

Execução de 19/10/2026, Go 1.27.2, macOS amd64, oito CPUs lógicas no host e Docker com duas vCPUs e 3,82 GiB. O ambiente local de desenvolvimento permaneceu ligado. Os resultados medem esse ambiente e incluem o custo do race detector.

Rajada: 300 BETs e 600 eventos por perfil.

| Cenário | HTTP req/s | Produção eventos/s | Publicação durante carga eventos/s | Pendentes após HTTP | Recuperação s | Até publicar req/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 processo, 100 carteiras | 39,47 | 78,94 | 3,82 | 571 | 9,50 | 17,54 |
| 3 processos, 100 carteiras | 57,98 | 115,97 | 41,36 | 386 | 3,93 | 32,90 |
| 3 processos, 1 carteira | 32,83 | 65,66 | 39,72 | 237 | 3,41 | 23,90 |

Carga sustentada: 200 BETs, alvo de 20/s durante aproximadamente dez segundos e 400 eventos por perfil.

| Cenário | HTTP req/s | Publicação durante carga eventos/s | Pendentes após HTTP | Recuperação s | Até publicar req/s |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 processo, 100 carteiras | 19,96 | 39,72 | 2 | 0,21 | 19,54 |
| 3 processos, 100 carteiras | 19,97 | 39,14 | 8 | 0,09 | 19,79 |
| 3 processos, 1 carteira | 19,71 | 38,63 | 8 | 0,09 | 19,52 |

Todos os perfis terminaram sem erro HTTP, divergência financeira, conflito SQL ou pendência e receberam todos os snapshots esperados. A rajada acumulou backlog; a carga sustentada manteve publicação próxima à produção nessa janela.

Outras execuções variaram consideravelmente. Os dados demonstram integridade e recuperação dentro do limite, sem estabelecer capacidade máxima, ganho estável de RPS ou escalabilidade previsível. O [histórico](historico/BENCHMARK_2026-10-19.md) preserva os ensaios anteriores e as métricas detalhadas.
