# Arquitetura

Concentrei no domínio as regras de dinheiro, carteira, transação, ledger e eventos para mantê-las independentes do transporte e do banco. Usei a aplicação para coordenar os casos de uso e os adapters para implementar PostgreSQL, HTTP, OIDC e SQS. Mantive a composição e o ciclo de vida das dependências no Fx.

## Uma unidade de confirmação

Criei `Manager.Within` para abrir uma transação `READ COMMITTED` e entregar um repositório vinculado ao mesmo `pgx.Tx`. Nenhuma gravação do caso de uso confirma sozinha. Abertura positiva confirma carteira, `OPENING`, crédito no ledger e dois eventos. Uma entrada SQS também confirma a inbox nesse commit.

Parsing, autorização e hash ocorrem antes da transação. O processamento procura a identidade financeira e, para operações novas, bloqueia a carteira com `FOR UPDATE` antes de reservar a transação. Essa ordem evita upgrades concorrentes dos locks adquiridos pelas chaves estrangeiras. `ON CONFLICT DO NOTHING` resolve a disputa de identidade; o perdedor consulta o registro confirmado e verifica o replay.

O lock de carteira serializa os movimentos, inclusive entre canais e processos. Carteiras diferentes usam locks diferentes. Deadlock e falha de serialização permitem até três tentativas da transação inteira, com espera curta e jitter. Indisponibilidade mantém o resultado anterior intacto e devolve `503` ao HTTP ou permite reentrega SQS.

Os timestamps de processamento são obtidos do banco depois de adquirir o lock. Assim, tempo aguardando outro movimento não produz lançamentos anteriores ao movimento que acabou de ser confirmado.

## Dinheiro e histórico

`Money` guarda centavos em `int64`, com moeda explícita. A entrada aceita uma string decimal não negativa com duas casas, sem notação científica, espaços ou zeros extras à esquerda. BRL, USD e EUR têm duas casas. Soma, subtração e inversão verificam overflow; não há conversão para ponto flutuante.

| Operação | Efeito |
| --- | --- |
| `OPENING` | Crédito interno positivo, versão inicial 1 |
| `BET` | Débito positivo, condicionado ao saldo |
| `WIN` | Crédito positivo; referência a BET é opcional |
| `LOSS` | Valor zero, sem ledger ou mudança de versão |
| `REFUND` | Crédito integral de uma BET processada |
| `ROLLBACK` de BET | Crédito integral |
| `ROLLBACK` de WIN ou REFUND | Débito integral, condicionado ao saldo atual |

Cada movimento posterior à abertura incrementa a versão uma vez. Uma carteira iniciada em zero não gera `OPENING` nem ledger; seu primeiro movimento fica na versão 2. `LOSS`, rejeição e replay preservam a versão.

O banco protege saldo não negativo, identidades financeiras, abertura única, aritmética e encadeamento do ledger, versão e correspondência entre transação e lançamento. Constraints adiadas verificam o estado final do commit. Transações terminais e lançamentos são imutáveis e não podem ser apagados ou truncados pela aplicação.

`REFUND` e `ROLLBACK` disputam uma única reversão bem-sucedida por referência. A referência precisa pertencer ao mesmo provedor, jogador, carteira, moeda e rodada. A operação original deve estar processada e o valor da reversão deve ser integral. A reversão de crédito com saldo insuficiente usa `REVERSAL_INSUFFICIENT_FUNDS`, distinto de `INSUFFICIENT_FUNDS` da aposta.

## Identidade e replay

Defini duas identidades únicas, ambas no escopo do provedor: `(providerId, idempotencyKey)` e `(providerId, externalTransactionId)`. Chave, ID externo e hash precisam coincidir para um replay. Trocar a chave do mesmo ID externo também é conflito.

O hash é SHA-256 do JSON UTF-8 canônico produzido com mapas de chaves ordenadas. Inclui `providerId`, `externalTransactionId`, `playerId`, `walletId`, `roundId`, `gameId`, `kind`, `money.amount`, `money.currency` e a referência quando preenchida. A ordenação também se aplica ao objeto `money`.

O valor monetário é serializado com duas casas; uma referência vazia equivale ao campo ausente. IDs UUID devem ser minúsculos no formato com hífens. Os demais textos são preservados, inclusive caixa; espaços nas bordas e controles são rejeitados. Chave de idempotência, ID interno, token, envelope, timestamps de transporte e correlação não participam do hash financeiro. HTTP e SQS usam a mesma função.

O resultado guarda o saldo observado no processamento. Um replay terminal retorna esse saldo, mesmo após outros movimentos. Uma pendência retorna seu estado persistido atual; sua conclusão posterior passa a ser o resultado consultável.

## Falhas transitórias e permanentes

Separei indisponibilidade de falha permanente para que um timeout não encerre uma operação que ainda pode ser processada. Perda de conexão, cancelamento, deadlock e falha de serialização continuam recuperáveis: o HTTP retorna `503` quando não há resultado confirmado, e o consumidor mantém a mensagem para reentrega.

Classifiquei como permanentes os erros SQL `23514`, `23503`, `22003` e `22P02`: restrição de integridade, chave estrangeira, overflow e representação inválida. A entrada já passou pelo domínio; uma violação dessas no adapter indica que a operação não pode ser confirmada conforme o contrato de persistência. A política é conservadora: registra a tentativa para investigação, sem repetir automaticamente uma operação que violou invariantes. Esses códigos são retornados pelo servidor e confirmam que a transação não teve sucesso; falhas de rede durante o commit não entram nessa classificação.

Tabela (`42P01`), coluna (`42703`) ou função (`42883`) ausentes são falhas operacionais de schema/deploy. Elas fazem rollback e retornam indisponibilidade, preservando a identidade para retry depois da correção das migrations. No SQS, a mensagem continua sujeita às cinco tentativas e pode chegar à DLQ antes do reparo; nesse caso, exige reenvio operacional após corrigir o schema. Não há garantia de recuperação automática após esgotar a política do broker.

Quando uma operação externa válida encontra um desses erros, primeiro concluo o rollback. Depois, abro outra transação, bloqueio a carteira e confiro novamente a identidade financeira. Se outra instância já tiver confirmado um resultado, devolvo esse resultado. Caso contrário, registro `FAILED` com `INFRASTRUCTURE_FAILURE`, sem saldo de resultado, movimento, ledger ou evento financeiro. Na entrada SQS, a inbox fica vinculada a essa auditoria no mesmo commit. Se o banco não permitir salvar a auditoria, devolvo indisponibilidade e preservo a possibilidade de nova tentativa.

`FAILED` é terminal, inclusive depois de corrigir a infraestrutura: replay não tenta movimentar a carteira. No HTTP, retorno `500` com o resultado persistido. No SQS, mantenho as reentregas até a DLQ para investigação; uma inbox concluída com falha permanente não autoriza remover essa mensagem. Uma referência pendente pode seguir a mesma política, desde que o worker ainda possua um lease válido.

O log da primeira falha registra o código SQL e os identificadores; a transação guarda o código de falha estável. Os eventos financeiros definidos neste projeto descrevem processamento, rejeição de negócio, pendência e mudança de saldo. Não publico um desses eventos como se uma falha de infraestrutura fosse uma rejeição de negócio.

## Referências fora de ordem

Uma referência ausente ou ainda pendente leva a `PENDING_REFERENCE`, sem movimento. O commit registra prazo final, próxima tentativa, número de tentativas e evento de pendência. A inbox pode ser concluída porque essa continuação já é durável no banco.

O worker reserva registros vencidos com `FOR UPDATE SKIP LOCKED`, token e prazo de lease. Antes de resolver, bloqueia a carteira e confere novamente estado, proprietário, validade da reserva e prazo da referência. Reservas expiradas podem ser retomadas por outro processo. A retomada preserva a correlação e a causação originais.

O padrão é dez tentativas, TTL de dez minutos e backoff a partir de um segundo, limitado a trinta segundos. Atingir o limite ou prazo gera `REFERENCE_NOT_FOUND`, saldo original da rejeição e evento, sem ledger. Uma referência já rejeitada gera `REFERENCE_NOT_PROCESSED`. Falhas técnicas não são confundidas com referência inexistente.

## Eventos e publicação

Cada evento tem tipo concreto, ID estável, aggregate, correlação, causação opcional, instante, versão 1 e dados tipados. Os construtores capturam o estado e retornam cópias dos bytes. A outbox guarda o JSON completo; a publicação não reconstrói dados a partir do saldo corrente.

Movimentos geram `WagerTransactionProcessed` e `WalletBalanceChanged`. `LOSS` gera apenas o primeiro. Rejeições geram `WagerTransactionRejected`; a primeira pendência gera `WagerTransactionPendingReference`. `OPENING` omite metadados externos inaplicáveis.

Adicionei à outbox uma sequência persistida e um índice das pendências por carteira para preservar a ordem de publicação entre instâncias. O publisher reserva apenas o primeiro evento ainda não publicado de cada carteira. Isso preserva a ordem também com vários publishers, mantendo carteiras diferentes em paralelo. Uma falha temporária pode atrasar eventos posteriores da mesma carteira.

O envio ocorre após o commit. Só o proprietário de um lease válido confirma `published_at`. Se o processo cair após enviar, outro publisher reenviará o mesmo snapshot e `eventId`; o protocolo é de entrega ao menos uma vez. Consumidores devem deduplicar por `eventId`, mesmo que FIFO suprima repetições dentro de sua janela de deduplicação.

Reagendar publicação ou referência também exige token e lease ainda válido. Os dois métodos conferem exatamente uma linha atualizada; ausência, conclusão, expiração ou retomada por outro worker retornam `ErrLeaseLost`. O proprietário antigo não pode adiar o trabalho reassumido.

Enquanto encontra trabalho, o publisher reserva o próximo lote imediatamente. `WORKER_INTERVAL` controla a espera quando não há candidatos ou a consulta falha. Estatísticas da outbox, pool e DLQ são coletadas em um ciclo separado, com pausa de um segundo, sem condicionar cada lote a uma consulta ao broker. A concorrência continua limitada a `WORKER_CONCURRENCY`, com um evento por carteira em cada reserva.

## SQS

| Fila | Uso |
| --- | --- |
| `wager-transactions.fifo` | Entrada financeira |
| `wager-transactions-dlq.fifo` | Mensagens sem tratamento após cinco recebimentos |
| `integration-events.fifo` | Eventos da outbox |

Na entrada, produtores usam `MessageGroupId=walletId` e um ID de deduplicação estável para a mensagem. Os testes também enviam IDs de transporte distintos para comprovar que a correção financeira independe da deduplicação temporária do broker. Na saída, `MessageGroupId=walletId` e `MessageDeduplicationId=eventId`.

O consumidor faz long polling de vinte segundos, recebe conforme sua capacidade e remove a mensagem somente após o commit. A inbox usa `(consumerName, messageId)` e SHA-256 do envelope canônico, incluindo instante UTC, chave e campos de negócio. A mesma identidade com outro envelope é conflito; mudar apenas o ID da mensagem ainda passa pela idempotência financeira.

Rejeições de negócio e pendências duráveis são tratamentos concluídos e permitem remover a mensagem. Uma auditoria em `FAILED` permanece sujeita à DLQ. JSON inválido, `OPENING` externo, conflito de identidade e falhas técnicas mantêm a mensagem para retry. O consumidor ajusta visibilidade com backoff e jitter; o broker encaminha à DLQ após cinco recebimentos. A inspeção e correção da DLQ são operacionais; não há reenvio automático de mensagens inválidas.

O canal SQS é de confiança da infraestrutura: a fila precisa ser privada, com escrita restrita aos produtores autorizados por IAM. O ambiente local usa uma policy da conta do emulador. JWT protege o HTTP; não existe token fictício embutido na mensagem.

## Limites operacionais

Cada processo usa até dez conexões por padrão. Os três workers têm concorrência limitada, sem fila ilimitada de goroutines. PostgreSQL controla leases, retries e identidade; não há mutex financeiro ou cache de idempotência por instância.

O padrão do lease e da visibilidade é trinta segundos. O publisher tem prazo de cinco segundos por item; o consumidor tem dez. Falhas de publicação usam backoff com intervalo base limitado a trinta segundos e jitter de até 25%, sem descartar eventos confirmados no banco. Mensagens SQS ficam sujeitas à política de DLQ.

O encerramento para de aceitar HTTP, aguarda requisições no prazo, cancela polling e jobs e fecha as conexões. Validei essa ordem com SIGTERM durante requisição bloqueada, mensagem em processamento e envio ao broker sem confirmação local. Trabalho interrompido é recuperado por rollback, expiração de lease ou visibilidade. Readiness verifica banco e filas; a inicialização também exige JWKS disponível. A verificação criptográfica usa RS256, emissor, audience, expiração, `nbf`, subject e permissões do cliente.

O readiness conjunto atende ao contrato do desafio: SQS indisponível retira a instância do tráfego, mesmo que uma chamada HTTP direta ainda pudesse confirmar saldo, ledger e outbox no PostgreSQL. A outbox permite acumular eventos duráveis, mas não altera a decisão do balanceador. Separar readiness de API e workers em produção exigiria definir limites de backlog, alertas e capacidade de recuperação; essa separação não está implementada.

`APP_ENV=production` remove os defaults locais de identidade, endpoint e credenciais. Exige issuer e JWKS explícitos por HTTPS, sem hosts de loopback ou aliases dos emuladores, e audience explícita. Recusa credenciais `test`. `SQS_ENDPOINT` vazio usa o endpoint AWS da região; um endpoint explícito precisa passar pelas mesmas verificações de HTTPS e host. O SDK usa sua cadeia padrão de credenciais, incluindo roles e tokens de sessão; não há override estático nessa modalidade. Endereços internos HTTPS de um provedor real continuam permitidos.

## Observabilidade e reconciliação

Os logs JSON identificam a etapa, a categoria do erro e os identificadores disponíveis. Falhas SQL registram SQLSTATE e constraint; falhas do SDK registram operação, código e identificador da resposta quando disponíveis. Mensagens brutas de exceção, tokens, secrets e payloads financeiros não são registrados. O procedimento de investigação e a interpretação desses campos estão em [DIAGNOSTICO.md](docs/DIAGNOSTICO.md).

`/metrics` exige o serviço interno. Expõe operações por tipo/status, duração, replays, conflitos de idempotência, conflitos SQL, retries, tamanho aproximado da DLQ, pendências e idade da outbox, publicações e divergências de reconciliação. Há histogramas do round trip de aquisição do lock e de aquisição de conexão mais `BEGIN`; eles incluem latência do banco. Métricas de runtime complementam o diagnóstico. Labels não contêm IDs.

Os histogramas `wager_outbox_claim_duration_seconds`, `wager_outbox_send_duration_seconds` e `wager_outbox_confirm_duration_seconds` separam reserva, `SendMessage` (incluindo retries do SDK) e confirmação. A amostragem do publisher também expõe conexões adquiridas, limite do pool e espera acumulada por pool vazio. Os valores de pool são locais ao processo; pendências da outbox são globais ao banco e não devem ser somadas entre réplicas.

Quantidade, timestamp mais antigo e horário atual da outbox são lidos juntos no PostgreSQL. A idade usa o mesmo relógio dos eventos persistidos, evitando valores negativos no benchmark ou atraso artificialmente zerado nas métricas quando o relógio da VM difere do host.

A reconciliação lê saldo, créditos, débitos e contagem em uma única consulta, sob a mesma visão do PostgreSQL. A soma usa `NUMERIC` no banco antes de retornar centavos inteiros. `difference` é saldo armazenado menos saldo reconstruído. O endpoint apenas informa a divergência, gera log e incrementa a métrica; não corrige o histórico.

## Falhas de teste

`FAULT_POINT` só é aceito com `APP_ENV=test` e `FAULT_TRANSACTION` preenchido. Os pontos `after_financial_commit`, `after_sqs_commit` e `after_event_publish` encerram o processo com código 97 para um ID externo específico. Os testes observam a falha e iniciam outro processo; as demais instâncias continuam independentes.

Não há dependência desses pontos para operação normal. As demonstrações, comandos e limites de validação estão em [CENARIOS.md](docs/CENARIOS.md).
