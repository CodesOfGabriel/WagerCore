# Verificação das auditorias

## Auditoria executada em outra máquina

Conferência local em 19/10/2026, sobre `692c5e98cb0190542a2abd48c446e408c3c6e285`. O relatório externo foi comparado ao código e reproduzido nos pontos que exigiam alteração.

| Ponto | Evidência e decisão |
| --- | --- |
| Dependências vulneráveis | `govulncheck` v1.9.0 reproduziu quatro vulnerabilidades alcançáveis em três módulos. Atualizados `go-jose/v4` para v4.1.4, `pgx/v5` para v5.9.2 e `x/text` para v0.41.0; `go mod tidy` ajustou apenas suas dependências transitivas. `make vuln` passou e foi incluído na CI, com a versão do scanner fixada no Makefile. |
| Lease e visibilidade | Os limites de produção já estavam corrigidos na base local, com deadlines compartilhados e testes por papel ativo. Mantidas as janelas curtas de `local`/`test`. A exigência adicional `REFERENCE_TTL > LEASE_DURATION` foi descartada: TTL e posse da tentativa são conferidos separadamente em `Resume`; o lease não prolonga o prazo de negócio. |
| Variáveis no Compose | Confirmada a omissão. Onze opções operacionais agora são interpoladas: pool, saúde, papéis, concorrência, intervalo de worker, intervalo da DLQ, lease, visibilidade, TTL, tentativas e backoff. Conferidos valores padrão e personalizados na configuração gerada. Ambiente local, endpoints internos, portas e credenciais dos emuladores continuam definidos na composição e foram documentados. |
| Snapshot inválido na outbox | Antes da correção, o PostgreSQL aceitou os doze casos negativos do novo teste: carteira divergente/ausente e campos com tipos incompatíveis com o publisher. A migration `000005` adiciona validação do envelope usado na publicação, incluindo igualdade de `data.walletId` com `wallet_id`. Ela valida o histórico existente e recusa atomicamente dados incompatíveis; não altera migrations aplicadas nem reescreve snapshots. Os construtores de domínio já produzem dados tipados; não foi reproduzida uma entrada inválida pela API. |
| Quarentena e alertas | O diagnóstico descreve o tratamento operacional e o uso de retries, idade e backlog. Não foi criado avanço automático sobre um evento inválido: isso exigiria uma política de reparo auditado e mudaria a garantia de ordem/entrega. A proteção adicionada atua antes de confirmar novos registros. |

As versões corrigidas foram conferidas nos avisos oficiais: [GO-2026-4945](https://pkg.go.dev/vuln/GO-2026-4945), [GO-2026-5004](https://pkg.go.dev/vuln/GO-2026-5004), [GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970) e [GO-2026-6629](https://pkg.go.dev/vuln/GO-2026-6629). O scanner mede alcance estático; um caminho apontado não comprova exploração por uma entrada externa. O aviso do pgx depende de protocolo simples com SQL e parâmetros específicos, combinação que não foi reproduzida na aplicação.

| Verificação desta rodada | Resultado |
| --- | --- |
| `make vuln` | Aprovado: zero vulnerabilidades alcançáveis e zero nos pacotes importados; duas nos módulos requeridos sem chamadas identificadas pelo scanner |
| `make check build` e `go mod verify` | Aprovados: testes, race detector, vet, binários e integridade dos módulos |
| Compose | Padrões e onze valores personalizados conferidos; endpoints internos e `APP_ENV=local` preservados |
| Proteção da outbox | Doze entradas inválidas rejeitadas e migração de histórico válido/inválido aprovados em 5,609s com race detector |
| Integração completa — rodada final | Aprovada em 204,746s com race detector: financeiro, concorrência, carga sustentada, mensageria, interrupções, indisponibilidade, shutdown e migrations |

A primeira execução completa encontrou `42P08` nos dois subcenários do novo teste de histórico: seu SQL de preparação usava o mesmo parâmetro como UUID e texto sem uma conversão consistente. A preparação foi corrigida com casts explícitos; os dois testes novos passaram isoladamente e a suíte completa passou na repetição. Os demais cenários da primeira rodada passaram. Os critérios de aplicação e os prazos dos testes foram preservados. Os containers e a rede de teste foram removidos após o sucesso; o ambiente local existente foi preservado.

Evidências locais ignoradas: `.local/audit3-vuln-before.log`, `.local/audit3-vuln-after.log`, `.local/audit3-check.log`, `.local/audit3-build-final.log`, `.local/audit3-compose.json`, `.local/audit3-outbox-before.log`, `.local/audit3-outbox-after.log`, `.local/audit3-integration.log` e `.local/audit3-integration-final.log`. Nenhum commit foi criado nesta rodada.

## Segunda auditoria

Base conferida: `e5c023d39c1a5819b9152d218e599a615b893858`, em 19/10/2026. O [workflow 39293060572](https://github.com/CodesOfGabriel/WagerCore/actions/runs/39293060572) foi consultado: `unit` e `integration` concluíram com sucesso e o SHA corresponde a essa base. As alterações da segunda revisão foram consolidadas no commit `692c5e9`; as evidências abaixo registram sua validação local.

| Ponto | Verificação e decisão |
| --- | --- |
| 3.1 — Custo no PostgreSQL | A BET coordena identidade, carteira, transação, ledger e dois eventos, com constraints e guards financeiros. Os cinco guards citados existem e as proteções foram preservadas. Os benchmarks identificam espera de lock/pool; não isolam custo de cada trigger. Otimização de SQL exige planos com `EXPLAIN (ANALYZE, BUFFERS)` e medição representativa antes de alterar invariantes. |
| 3.2 — Envio individual | `SendMessage` por evento é uma limitação conhecida. Batching continua opcional: exige tratar respostas parcialmente bem-sucedidas, retries individuais e ordem por carteira. A publicação mantém snapshot e `eventId` estáveis. |
| 3.3 — Janelas inconsistentes | Configuração de produção valida os prazos completos dos workers ativos. Lease deve superar reserva + processamento + limpeza; visibilidade deve superar processamento + limpeza. Os deadlines são compartilhados entre validação e execução. `local`/`test` preservam janelas curtas. |
| 3.4 — Polling da DLQ | DLQ tem ciclo independente, padrão 30s, configurável por `DLQ_POLL_INTERVAL` entre 1s e 1h. Outbox/pool mantêm pausa de 1s. Testes verificam periodicidade, atualização da métrica e cancelamento. |
| 3.5 — `FAILED` terminal | Política preservada e procedimento operacional esclarecido. `FAILED` é falha técnica auditada; `REJECTED` é recusa de negócio. Corrigir infraestrutura ou reenviar a mesma identidade não reabre um terminal. |
| 4 — Organização e apresentação | Workers separados em runtime, publisher, consumer e referências, sem novas interfaces. Documentação passou a tom objetivo; README prioriza os documentos de uso; histórico foi movido para anexos. Dividir aplicação/repositório permanece opcional, sem reescrever os fluxos financeiros nesta entrega. |
| 5 — Nome da medição | O JSON usa `financialToPublishedSeconds` e `financialToPublishedRequestsPerSecond`; os logs e a documentação indicam término na confirmação de publicação. Conferência no destino não é processamento downstream. Resultados históricos mantêm sua identificação. |

A justificativa do envio individual considera o contrato de [SendMessageBatch](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_SendMessageBatch.html): uma resposta HTTP 200 pode conter falhas por item. Sua adoção exige testes próprios de sucesso parcial e ordem.

## Validação desta revisão

| Verificação | Resultado |
| --- | --- |
| `make check build` | Aprovado: testes, race detector, vet e os dois binários |
| `make test-integration` — rodada final | Aprovado em 203,517s, incluindo três processos, falhas, recuperação e carga sustentada curta |
| Concorrência isolada | Cinco execuções consecutivas de `TestThreeProcessesConcurrentBets` aprovadas em 29,742s |
| Inicialização isolada | Build e quatro migrations; PostgreSQL, Keycloak, LocalStack e API saudáveis, sem volumes ou `.env` anteriores |
| Fluxo do README | Tokens, abertura de 100, BET de 25, replay com saldo 75, dois lançamentos reconciliados e quatro eventos publicados; zero pendências; acessos indevidos bloqueados |
| Configuração no Compose | `DLQ_POLL_INTERVAL=1m` propagado à configuração da API |
| Arquivos versionáveis | 89 arquivos conferidos: nenhum padrão pesquisado de credencial real, artefato temporário ou arquivo ignorado já rastreado; exemplos locais revisados |

A primeira rodada da integração retornou um `503` no cenário de 50 replays concorrentes, enquanto o build do ambiente limpo também estava ativo. A saída original não continha os logs dos processos para confirmar a causa, e o PostgreSQL não registrou timeout ou deadlock correspondente. O teste passou a incluir esses logs em respostas inesperadas. A falha não se repetiu nas cinco rodadas isoladas nem na suíte final; nenhum prazo de requisição ou critério financeiro foi relaxado. A causa da ocorrência inicial não foi confirmada.

As evidências locais ficam em `.local/audit2-check.log`, `.local/audit2-integration.log`, `.local/audit2-integration-final.log`, `.local/audit2-concurrency-repeat.log`, `.local/audit2-clean-startup.log`, `.local/audit2-smoke.json` e `.local/audit2-versionable-scan.json`. Esses arquivos permanecem ignorados. Os containers de teste e o projeto temporário `wagercore-audit2-clean` foram removidos após a conferência.

## Entrega

O SHA e a tag definitivos dependem do commit do responsável pela entrega. A CI confirmada acima vale para `e5c023d`; a revisão local deve passar por nova CI depois de publicada. A conferência de arquivos versionáveis cobre credenciais reais e artefatos temporários; os valores de Keycloak/AWS/PostgreSQL versionados são exemplos locais.

O [registro anterior](historico/AUDITORIA_ANTERIOR.md), os [cenários](CENARIOS.md) e o [diagnóstico](DIAGNOSTICO.md) são material complementar. O funcionamento e a execução estão no [README](../README.md), na [arquitetura](../ARCHITECTURE.md), na [API](API.md) e nos [testes](TESTES.md).
