# Primeira revisão técnica — 19/10/2026

Base inicial: `d13e6e6`. As correções foram publicadas em `e5c023d`. O registro integral permanece no [arquivo desse commit](https://github.com/CodesOfGabriel/WagerCore/blob/e5c023d39c1a5819b9152d218e599a615b893858/docs/AUDITORIA.md).

Foram corrigidos o intervalo obrigatório entre lotes ocupados do publisher, retries que não conferiam linhas alteradas, benchmark sem drenagem, defaults locais em produção, classificação terminal de falhas de schema e medição de idade com relógios diferentes. O readiness conjunto foi mantido e documentado.

| Verificação local | Resultado |
| --- | --- |
| `make check build` | Testes, race detector, vet e binários aprovados |
| `make test-integration` | Aprovado em 229,999s |
| `make test-failover` | Aprovado em 114,305s |
| Rajada e regressão do relógio | Aprovadas em 92,141s; 600 eventos por perfil, recuperação de 3,41–9,50s |
| Carga sustentada | Aprovada em 71,538s; 400 eventos por perfil, recuperação de 0,09–0,21s |
| Instalação isolada | Quatro migrations, serviços saudáveis, abertura de 100, BET de 25, replay com saldo 75, dois lançamentos reconciliados e quatro eventos publicados |

Os cenários de carga terminaram sem erro, divergência ou pendência. Os resultados variaram entre execuções e não foram apresentados como ganho estável de throughput. Os números e limites estão no [histórico dos benchmarks](BENCHMARK_2026-10-19.md).

O projeto temporário e seus volumes foram removidos após os testes. Logs, JSONs e fontes exportados ficaram em `.local/`, fora do versionamento. A CI de `d13e6e6` foi confirmada na primeira revisão; a CI de `e5c023d` foi confirmada na [segunda](../AUDITORIA.md).
