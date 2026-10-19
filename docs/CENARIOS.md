# Cenários de resiliência e desempenho

Este documento define as demonstrações que serão construídas depois dos fluxos financeiros. Os comandos de execução ficam no README; as decisões de domínio e persistência ficam no ARCHITECTURE.

Os cenários desta página estão planejados. Os testes atuais cobrem domínio e restrições no PostgreSQL, conforme descrito no README.

## Distribuição de HTTP

Adicionar Traefik quando abertura e BET já estiverem validadas. Ele será a entrada HTTP de três instâncias Go e usará `/health/ready` para retirar uma instância indisponível da rotação. SQS continuará entregando mensagens diretamente aos consumidores Go.

As réplicas usarão portas internas, sem `container_name` fixo. Cada resposta terá um identificador de instância para permitir conferir a distribuição. O cliente poderá reenviar uma operação para qualquer réplica. Sessões não terão afinidade com uma instância, e o proxy não fará retry automático de POST.

A descoberta Docker terá acesso restrito à API do daemon, por meio de um socket proxy com as permissões necessárias. Essa restrição faz parte da [configuração de segurança recomendada pelo Traefik](https://doc.traefik.io/traefik/reference/install-configuration/providers/docker/#docker-api-access). O dashboard administrativo, as métricas e os serviços de infraestrutura ficarão nas redes de acesso previstas para cada um. A aplicação continuará validando a autorização dos endpoints de negócio.

O teste deve identificar pelo menos três processos independentes, encerrar uma instância, observar novas requisições nas outras duas e repetir os replays após a recuperação.

## Interrupções controladas

Preparar pontos de interrupção habilitados apenas no ambiente de teste. Cada ponto deve indicar a etapa alcançada antes de encerrar o processo.

| Ponto da interrupção | Resultado que deve ser observado |
| --- | --- |
| Antes do commit financeiro | Nenhum efeito parcial; reenvio processa uma vez |
| Depois do commit e antes da resposta HTTP | Replay devolve o resultado persistido |
| Depois do commit e antes de remover a mensagem SQS | Reentrega comprovada, sem outro movimento |
| Depois do commit e antes de publicar a outbox | Outra instância publica o evento pendente |
| Depois de publicar e antes de confirmar a outbox | Republicação com o mesmo `eventId` |
| Com uma referência pendente ou lease ativo | Outra instância retoma o trabalho após a expiração da reserva |

O futuro alvo `make test-failover` reunirá preparação, operações concorrentes, interrupção, recuperação e reconciliação. O resultado deverá ser calculado a partir do banco e das mensagens observadas.

O relatório mostrará quantidade de instâncias, processo interrompido, efeitos duplicados, saldos negativos, eventos confirmados sem recuperação, divergências de ledger e pendências encerradas incorretamente. Qualquer ocorrência invalida o cenário. Não serão usados números ilustrativos como resultado de execução.

## Concorrência e uso do PostgreSQL

Manter validação de credenciais, parsing e hash fora da transação financeira. Dentro dela ficam reserva da identidade, locks, domínio e gravações. Chamadas ao IdP e publicações SQS ficam fora do período em que a carteira está bloqueada.

O pool terá um limite configurável por instância. Dez conexões é um ponto inicial de medição, não uma meta de desempenho. O orçamento total considerará as três instâncias, workers, migrations e conexões do IdP.

Validar os índices de consultas de resultado original, ledger, pendências e outbox com `EXPLAIN (ANALYZE, BUFFERS)` em dados de teste. A reserva da outbox precisa considerar leases expirados. `pg_stat_statements` poderá complementar a análise quando as consultas estiverem definidas.

As duas apostas de 80.00 sobre saldo de 100.00 precisam ser serializadas por carteira. Ao mesmo tempo, operações em carteiras distintas devem avançar. O teste deve medir tanto a correção quanto a espera por locks e por conexão.

## Workers e retry

Consumidores SQS, publishers e processamento de referências terão limites próprios de concorrência e lote. A busca por mais trabalho acompanhará a capacidade de processamento para limitar filas em memória.

O consumidor usará long polling, cujo limite é de 20 segundos segundo a [documentação do SQS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-short-and-long-polling.html). `MessageGroupId` será definido por carteira. Locks PostgreSQL continuarão coordenando o processamento que chega por HTTP.

| Situação | Tratamento a verificar |
| --- | --- |
| Saldo insuficiente | Rejeição persistida, sem retry de negócio |
| Referência ausente | Retry persistente com backoff até o limite definido |
| Falha transitória de infraestrutura | Retry limitado com backoff e jitter |
| Deadlock SQL | Nova tentativa da transação inteira, com limite |
| Mensagem definitivamente inválida | Destino conforme a política documentada de DLQ |
| Publicação sem confirmação local | Mesmo evento e mesmo identificador na nova tentativa |

Os testes devem reduzir a disponibilidade do banco ou broker e verificar limites de trabalho em execução, recuperação e encerramento dentro do prazo. Valores iniciais de concorrência serão definidos com a implementação e medidos antes de qualquer ajuste.

## Métricas

Instrumentar resultados por status, duração de processamento, replays, espera por lock, aquisição de conexão, tentativas dos workers, atraso da outbox e divergências de reconciliação. Medir uso do pool para distinguir falta de conexões de contenção por carteira.

Labels usarão dimensões limitadas, como tipo e status. Identificadores de carteira, transação e mensagem ficarão nos logs, seguindo a [orientação de cardinalidade do Prometheus](https://prometheus.io/docs/practices/naming/#labels). O monitoramento de CPU, memória e containers poderá complementar essas medições.

## Integração contínua

Criar um workflow de GitHub Actions com formatação, dependências, `go vet`, testes e race detector. Um job de integração preparará PostgreSQL, Keycloak e LocalStack, executará migrations e publicará o resultado real dos testes.

Na CI, a suíte PostgreSQL usará o comando de integração já disponível. A inclusão do IdP e SQS acompanhará a implementação desses componentes. Concorrência, failover e benchmark terão comandos próprios quando os cenários estiverem completos.

## Benchmark

Executar depois dos testes de integridade e recuperação. Registrar ambiente, versões, recursos, duração, carga e proporção de operações. Comparar os cenários com a mesma carga total:

| Cenário | O que permite comparar |
| --- | --- |
| Uma instância e 100 carteiras | Referência de processamento |
| Três instâncias e 100 carteiras | Efeito da distribuição em carteiras independentes |
| Três instâncias e uma carteira disputada | Contenção necessária e preservação do saldo |

Coletar throughput, taxa de erros, p50, p95, p99, espera por conexão, conflitos, atraso da outbox e divergências de ledger. A comparação precisa explicar as diferenças observadas. Não há meta mínima de RPS no desafio.

## Ordem de entrega

1. Concluir abertura e BET autenticadas, com atomicidade e idempotência.
2. Demonstrar concorrência financeira entre três processos.
3. Completar operações, SQS e recuperação com workers limitados.
4. Executar failover pelo Traefik e interrupções controladas, com métricas e CI.
5. Medir desempenho e registrar os resultados com a metodologia usada.
