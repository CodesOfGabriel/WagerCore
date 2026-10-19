# Teste de carga

Criei `TestFinancialLoad` para comparar a mesma carga HTTP com uma e três instâncias Go, usando PostgreSQL e SQS compartilhados. Mantive a medição separada dos testes de correção para não executar carga em toda integração.

```sh
make benchmark
```

O comando prepara os serviços de teste, executa os três cenários e salva `.local/benchmark.json`. Se passar, remove os containers; em caso de falha, mantém o ambiente para investigação, conforme a política dos testes.

Para alterar a carga:

```sh
BENCHMARK_REQUESTS=600 BENCHMARK_CONCURRENCY=30 make benchmark
```

Aceito de 1 a 1000 requisições por cenário e de 1 a 50 clientes simultâneos. `BENCHMARK_REPORT` permite alterar o destino do JSON; um caminho absoluto evita ambiguidade, pois o teste executa em `tests/integration`.

## Metodologia

| Cenário | Processos Go | Carteiras |
| --- | ---: | ---: |
| `one_process_100_wallets` | 1 | 100 |
| `three_processes_100_wallets` | 3 | 100 |
| `three_processes_one_wallet` | 3 | 1 |

Uso 300 BETs de `0.01 BRL` por cenário, distribuídas entre vinte clientes no padrão. Cada operação tem uma identidade nova e as chamadas são direcionadas diretamente às instâncias. Cada carteira começa com `100000.00 BRL`, evitando rejeições por falta de saldo.

Antes de cronometrar, crio as carteiras, aguardo a publicação das aberturas, consumo esses eventos e capturo as métricas iniciais. A medição inclui construção e envio da requisição HTTP e espera pela resposta. Autenticação inicial, provisionamento, abertura, reconciliação e encerramento ficam fora do intervalo medido.

Cada processo usa pool de dez conexões e um publisher com intervalo de 200 ms. O teste usa o mesmo binário com race detector dos cenários de integração. Portanto, os números incluem essa instrumentação e não estimam a capacidade de um binário de produção.

O JSON contém throughput HTTP, p50/p95/p99, erros, divergências, conflitos SQL, tempo acumulado de lock e de início de transação, além de quantidade e idade das pendências da outbox ao final da carga. Os tempos acumulados de métricas são a diferença entre as leituras anterior e posterior; podem superar o tempo de parede porque várias requisições aguardam simultaneamente.

Confiro saldo, versão e soma do ledger de todas as carteiras. Uma resposta malsucedida ou divergência faz o teste falhar. O contador de conflitos registra deadlock, serialização ou versão; espera normal por lock não incrementa esse contador.

## Execução local de 19/10/2026

Executei com Go 1.27.2 em macOS amd64, oito CPUs lógicas no host e Docker com duas vCPUs e aproximadamente 3,82 GiB de memória. As imagens usadas foram PostgreSQL 17.6 Alpine, Keycloak 26.4.0 e LocalStack 4.4.0. O relatório registra os parâmetros e o instante da execução.

| Cenário | Requisições/s | p50 ms | p95 ms | p99 ms | Erros | Divergências | Outbox pendente | Idade da mais antiga s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 processo, 100 carteiras | 70,92 | 280,71 | 382,83 | 417,49 | 0 | 0 | 584 | 4,08 |
| 3 processos, 100 carteiras | 51,10 | 225,01 | 1327,02 | 2553,15 | 0 | 0 | 507 | 4,63 |
| 3 processos, 1 carteira | 33,94 | 478,44 | 1563,17 | 2431,29 | 0 | 0 | 534 | 6,31 |

As três execuções terminaram sem conflito SQL. No cenário de uma carteira disputada, o tempo acumulado de lock chegou a 158,52 segundos, compatível com várias requisições aguardando o mesmo recurso durante um intervalo de 8,84 segundos.

Três processos não aumentaram o throughput observado nessa execução. Eles compartilharam o mesmo banco, broker e recursos limitados da VM. Uma medição isolada não permite atribuir a diferença a uma única causa nem extrapolar escalabilidade para outra máquina.

O volume pendente da outbox mostra que a publicação não acompanhou a entrada HTTP durante a carga. Essa é uma medição de confirmação financeira; a entrega de todos os eventos não integra o throughput informado. Coleto o atraso separadamente e descarto os dados temporários ao encerrar cada cenário. Os testes de recuperação verificam a entrega e a retomada da outbox.

Antes de ajustar concorrência ou pools, repetiria a carga em ambiente controlado e examinaria os planos SQL e as métricas de publicação. Não há meta mínima de RPS no desafio.
