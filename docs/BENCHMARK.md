# Teste de carga

Criei `TestFinancialLoad` para comparar a mesma carga HTTP com uma e três instâncias Go, usando PostgreSQL e SQS compartilhados. A medição distingue confirmação financeira, publicação durante a carga e recuperação do backlog. Uma versão curta de carga sustentada também executa na integração e na CI.

```sh
make benchmark
make benchmark-e2e
```

Os comandos preparam os serviços de teste e executam os três cenários. `benchmark` envia uma rajada de 300 requisições e salva `.local/benchmark.json`; `benchmark-e2e` distribui 200 requisições a 20/s e salva `.local/benchmark-e2e.json`. Se passar, remove os containers; em caso de falha, mantém o ambiente para investigação, conforme a política dos testes.

Para alterar a carga:

```sh
BENCHMARK_REQUESTS=600 BENCHMARK_CONCURRENCY=30 make benchmark
BENCHMARK_REQUESTS=600 BENCHMARK_RATE=20 BENCHMARK_DRAIN_TIMEOUT=90s make benchmark-e2e
```

Aceito de 1 a 1000 requisições por cenário e de 1 a 50 clientes simultâneos. `BENCHMARK_RATE`, quando definido, limita o início dos envios a 1–1000/s; a taxa efetiva pode ser menor se os clientes ficarem ocupados. `BENCHMARK_DRAIN_TIMEOUT` limita a recuperação após a carga, de 1s a 2m, com padrão de 60s. `BENCHMARK_REPORT` permite alterar o destino do JSON; um caminho absoluto evita ambiguidade, pois o teste executa em `tests/integration`.

## Metodologia

| Cenário | Processos Go | Carteiras |
| --- | ---: | ---: |
| `one_process_100_wallets` | 1 | 100 |
| `three_processes_100_wallets` | 3 | 100 |
| `three_processes_one_wallet` | 3 | 1 |

Uso 300 BETs de `0.01 BRL` por cenário, distribuídas entre vinte clientes no padrão. Cada operação tem uma identidade nova e as chamadas são direcionadas diretamente às instâncias. Cada carteira começa com `100000.00 BRL`, evitando rejeições por falta de saldo.

Antes de cronometrar, crio as carteiras, aguardo a publicação das aberturas, consumo esses eventos e capturo as métricas iniciais. A medição inclui construção e envio da requisição HTTP e espera pela resposta. Autenticação inicial, provisionamento, abertura, reconciliação e encerramento ficam fora do intervalo medido.

Cada processo usa pool de dez conexões, publisher com concorrência quatro e pausa de 200 ms somente quando não encontra trabalho ou a reserva falha. Com candidatos disponíveis, o próximo lote é imediato. O teste usa o mesmo binário com race detector dos cenários de integração. Portanto, os números incluem essa instrumentação e não estimam a capacidade de um binário de produção.

O JSON contém throughput HTTP, p50/p95/p99, erros, divergências, conflitos SQL, tempo acumulado de lock e de início de transação, além de quantidade e idade das pendências da outbox ao final da carga. Também registra:

- Eventos produzidos por segundo e eventos confirmados como publicados por segundo durante a carga, calculados a partir do backlog observado ao terminar o HTTP.
- Tempo adicional para zerar a outbox, pendências após essa recuperação e throughput desde o primeiro envio até confirmar toda a publicação.
- Quantidade de snapshots recebidos no SQS. Cada `eventId` e payload deve corresponder ao banco, mantendo a ordem por carteira. Duplicatas com os mesmos bytes são deduplicadas na conferência.
- Tempos médios de reserva, `SendMessage` e confirmação, além de espera acumulada por pool vazio. As médias incluem falhas e retries; a estatística do pool é amostrada pelo publisher.

O throughput end-to-end termina na confirmação de publicação no banco. A leitura e conferência dos eventos no destino acontece depois, fora desse cronômetro. Os tempos acumulados de métricas são a diferença entre as leituras anterior e posterior; podem superar o tempo de parede porque várias requisições aguardam simultaneamente.

Confiro saldo, versão e soma do ledger de todas as carteiras. Uma resposta malsucedida, divergência, backlog que não zera no prazo ou snapshot ausente/modificado/fora de ordem faz o teste falhar. O contador de conflitos registra deadlock, serialização ou versão; espera normal por lock não incrementa esse contador. O timeout é um critério de recuperação deste ensaio local, sem estabelecer uma meta de produção.

`TestOutboxSustainedLoad` usa o mesmo fluxo com 60 requisições a 20/s por perfil e limite de trinta segundos para recuperação. Seu relatório fica em diretório temporário e o resumo aparece nos logs com `-v`.

## Execução anterior à correção, em 19/10/2026

Executei com Go 1.27.2 em macOS amd64, oito CPUs lógicas no host e Docker com duas vCPUs e aproximadamente 3,82 GiB de memória. As imagens usadas foram PostgreSQL 17.6 Alpine, Keycloak 26.4.0 e LocalStack 4.4.0. O relatório registra os parâmetros e o instante da execução.

| Cenário | Requisições/s | p50 ms | p95 ms | p99 ms | Erros | Divergências | Outbox pendente | Idade da mais antiga s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 processo, 100 carteiras | 70,92 | 280,71 | 382,83 | 417,49 | 0 | 0 | 584 | 4,08 |
| 3 processos, 100 carteiras | 51,10 | 225,01 | 1327,02 | 2553,15 | 0 | 0 | 507 | 4,63 |
| 3 processos, 1 carteira | 33,94 | 478,44 | 1563,17 | 2431,29 | 0 | 0 | 534 | 6,31 |

As três execuções terminaram sem conflito SQL. No cenário de uma carteira disputada, o tempo acumulado de lock chegou a 158,52 segundos, compatível com várias requisições aguardando o mesmo recurso durante um intervalo de 8,84 segundos.

Três processos não aumentaram o throughput observado nessa execução. Eles compartilharam o mesmo banco, broker e recursos limitados da VM. Uma medição isolada não permite atribuir a diferença a uma única causa nem extrapolar escalabilidade para outra máquina.

O volume pendente da outbox mostra que a publicação não acompanhou a entrada HTTP durante aquela carga. O benchmark anterior encerrava sem exigir recuperação; esses números históricos medem somente confirmação financeira. Cada lote também aguardava 200 ms e uma consulta de DLQ antes da próxima reserva, mesmo com trabalho disponível. A versão atual remove essas esperas do caminho ocupado e exige drenagem e conferência no destino antes de encerrar o cenário.

A concorrência e o pool foram preservados. As novas métricas permitem avaliar reserva, envio e saturação antes de ajustar esses limites. Não há meta mínima de RPS no desafio.

## Rajada após a correção, em 19/10/2026

`make benchmark` passou e o ensaio foi repetido após corrigir a idade para usar somente o relógio do PostgreSQL. A repetição, junto com `TestOutboxStatsUseDatabaseClock`, passou em 92,141 segundos de suíte. O relatório final foi gravado às 14:12, horário de São Paulo, com Go 1.27.2, macOS amd64, oito CPUs lógicas no host e race detector. O Docker continuou com duas vCPUs e aproximadamente 3,82 GiB; o ambiente local de desenvolvimento permaneceu ligado. Foram 300 BETs e 600 eventos por perfil, com vinte clientes, pool dez e publisher de concorrência quatro.

| Cenário | HTTP req/s | Produção eventos/s | Publicação durante carga eventos/s | Pendentes após HTTP | Recuperação s | End-to-end req/s | Pendentes finais |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 processo, 100 carteiras | 39,47 | 78,94 | 3,82 | 571 | 9,50 | 17,54 | 0 |
| 3 processos, 100 carteiras | 57,98 | 115,97 | 41,36 | 386 | 3,93 | 32,90 | 0 |
| 3 processos, 1 carteira | 32,83 | 65,66 | 39,72 | 237 | 3,41 | 23,90 | 0 |

Todos os perfis receberam e conferiram os 600 snapshots no SQS, com zero erros HTTP, divergências financeiras ou conflitos SQL. A rajada ainda produz eventos mais depressa do que são publicados durante o HTTP; a recuperação posterior agora é medida e exigida antes do encerramento. Esses resultados não demonstram capacidade para uma taxa de entrada ilimitada.

No perfil de um processo, as médias de reserva, envio e confirmação foram 50,13 ms, 35,14 ms e 19,84 ms, respectivamente. A espera acumulada por pool vazio foi 89,79 segundos; ela soma espera de várias operações e pode superar o tempo de parede. Isso permite investigar contenção junto com o envio ao broker. O relatório completo fica em `.local/benchmark.json`.

O primeiro ensaio após a mudança do publisher registrou recuperação de 4,52/1,98/2,32 segundos e HTTP de 106,65/64,80/41,32 req/s, na mesma ordem dos perfis. A variação na repetição mostra que estes números locais não estabelecem ganho de RPS nem escalabilidade previsível. A evidência comum às duas execuções é integridade e recuperação completa dentro do limite; não se trata de uma comparação A/B controlada.

## Carga sustentada após a correção, em 19/10/2026

`make benchmark-e2e` passou. A repetição final, após o ajuste de idade, também passou em 71,538 segundos de suíte, reutilizando os serviços isolados já preparados. O relatório foi gravado às 14:14, horário de São Paulo, em `.local/benchmark-e2e.json`. Foram 200 BETs por perfil, com taxa alvo de 20/s durante aproximadamente dez segundos.

| Cenário | HTTP req/s | Produção eventos/s | Publicação durante carga eventos/s | Pendentes após HTTP | Recuperação s | End-to-end req/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 processo, 100 carteiras | 19,96 | 39,92 | 39,72 | 2 | 0,21 | 19,54 |
| 3 processos, 100 carteiras | 19,97 | 39,93 | 39,14 | 8 | 0,09 | 19,79 |
| 3 processos, 1 carteira | 19,71 | 39,42 | 38,63 | 8 | 0,09 | 19,52 |

Cada perfil recebeu os 400 snapshots, sem erro, divergência ou conflito SQL e com zero pendências finais. A publicação ficou próxima da produção nessa janela; o backlog restante zerou em até 0,22 segundo. As idades medidas pelo relógio do banco ficaram não negativas. Isso demonstra a recuperação nessa carga local limitada, sem extrapolar capacidade para outras taxas ou infraestrutura.
