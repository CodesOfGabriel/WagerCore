# Arquitetura

## Organização

A aplicação utiliza um único binário Go. API HTTP e workers compartilharão os mesmos casos de uso e o mesmo PostgreSQL. Cada instância terá sua própria memória e suas próprias conexões.

O domínio fica em `internal/domain` e depende apenas da biblioteca padrão. Os adapters tratam protocolos e persistência. `internal/bootstrap` concentra a composição com Fx. Pacotes de aplicação e novos adapters serão criados conforme os casos de uso forem implementados.

## Implementado

### Dinheiro

`Money` guarda centavos em `int64` e moeda em um campo privado. Os valores são imutáveis: operações devolvem um novo valor e não alteram seus operandos.

`NewMoney` e a entrada JSON aceitam apenas valores não negativos em formato decimal canônico, como `0.00` ou `25.00`. São recusados sinais, espaços, zeros extras à esquerda, notação científica e quantidades diferentes de duas casas decimais. A entrada não passa por arredondamento ou normalização.

`MoneyFromMinorUnits` permite valores negativos para diferenças internas e reidratação. A serialização também permite negativos para respostas como a diferença de reconciliação. A entrada JSON continua recusando negativos; ela representa o contrato de entrada financeira, não a reidratação do banco.

O intervalo interno vai de `-92233720368547758.08` a `92233720368547758.07`. O construtor decimal externo aceita de `0.00` ao limite positivo. Parsing, soma, subtração e negação verificam overflow. Comparações não calculam diferenças que possam estourar o intervalo.

BRL, USD e EUR são as moedas suportadas pelo tipo, todas com duas casas decimais. Os cenários financeiros principais usarão BRL. Operações entre moedas diferentes retornam `ErrCurrencyMismatch`. O valor zero de Go para `Money` tem moeda inválida e é recusado por validação, cálculo e serialização.

O JSON tem a forma `{"amount":"25.00","currency":"BRL"}`. PostgreSQL armazena unidades mínimas em `BIGINT` e código de moeda explícito.

### Entidades

`Wallet` mantém saldo, identidade, versão e timestamps privados. Débito e crédito validam valor positivo, moeda, saldo disponível, tempo e limites numéricos antes de alterar o agregado. Uma operação recusada preserva o estado anterior.

`WagerTransaction` separa os dados de entrada do resultado persistido. O construtor externo recusa `OPENING`; a abertura possui construtor próprio. `Process`, `Reject`, `AwaitReference` e `Fail` validam as transições. Estados terminais não aceitam outra transição. Resultado e snapshots são copiados para impedir alteração por referências compartilhadas.

`WalletLedgerEntry` é imutável e valida direção, valor, moeda e a igualdade entre saldo anterior, movimento e saldo posterior. `walletVersion` identifica a posição financeira do lançamento.

A criação recebe os dados iniciais. A reidratação recebe um snapshot validado e preserva saldo, versão, estado e instantes, sem aplicar operações ou emitir eventos. IDs de entidades usam UUID não nulo em formato canônico minúsculo. Identificadores externos têm até 255 bytes e são recebidos sem espaços nas extremidades.

### Schema financeiro

`wallets` impõe unicidade de jogador e moeda, saldo não negativo e identidade imutável. `wager_transactions` distingue abertura interna de operações externas e impõe unicidade por provedor e chave, por provedor e ID externo e por abertura da carteira. Uma referência admite apenas uma reversão processada, considerando `REFUND` e `ROLLBACK` em conjunto.

O ledger exige uma operação financeira correspondente, com a mesma carteira, moeda e valor. Cada inserção bloqueia a carteira e valida a continuidade de saldo e versão em relação ao último lançamento. `UPDATE`, `DELETE` e `TRUNCATE` são recusados. Transações financeiras também são preservadas contra exclusão, e seus dados de entrada são imutáveis.

Triggers diferidos conferem o estado final no commit: saldo e versão da carteira correspondem ao último lançamento, operações financeiras processadas têm ledger, e resultados persistidos correspondem ao saldo posterior. A abertura positiva exige `OPENING` do mesmo valor; abertura zero dispensa esse registro. `LOSS` e rejeições não admitem lançamentos.

Essas verificações entre tabelas usam [constraint triggers do PostgreSQL](https://www.postgresql.org/docs/17/sql-createtrigger.html). As operações SQL precisam compartilhar a transação para que o estado completo seja verificado no commit. Erros de commit devem ser tratados como falha da operação inteira.

As referências resolvidas validam tipo, estado, provedor, jogador, carteira, moeda, rodada e ID externo. Reversões também exigem igualdade de valor. Os campos de retry e lease estão preparados para o worker de referências, que será implementado em outra etapa.

### Migrations

O comando `migrate` utiliza `pgx` e os arquivos SQL embutidos no binário. `schema_migrations` guarda versão, nome, checksum SHA-256 do par de arquivos e instante de aplicação. O runner verifica o histórico antes de aplicar ou reverter versões.

A execução inteira ocorre em uma transação SQL, incluindo DDL e registro da versão. Um advisory lock coordena apenas execuções de migrations. Esse lock não participa do processamento financeiro por carteira. `down` reverte uma versão por execução.

O Compose executa as migrations em um processo separado e só inicia a API se esse processo terminar com sucesso. A suíte de integração usa schemas temporários em um PostgreSQL separado e verifica aplicação, reaplicação, reversão, checksum e rollback de DDL.

### Inicialização e encerramento

Fx organiza os módulos de configuração, PostgreSQL e HTTP. Os construtores recebem dependências explicitamente. O pool registra seu hook antes do servidor: a aplicação verifica a conexão antes de abrir a porta HTTP. Ao encerrar, o servidor para de aceitar conexões e aguarda as requisições antes de fechar o pool.

O prazo de início é de 10 segundos e o de encerramento é de 15 segundos. O Compose concede 20 segundos antes do encerramento forçado. Se o servidor HTTP falhar em execução, a aplicação solicita encerramento com código de saída diferente de zero.

Os logs da aplicação e do Fx usam JSON com `slog`. A URL de conexão e as credenciais não são registradas. As respostas de saúde não expõem detalhes internos de falhas.

Os health checks representam somente os componentes disponíveis nesta etapa: liveness do processo e readiness de PostgreSQL. Readiness ainda precisa incorporar SQS.

## Decisões para as próximas etapas

O domínio e o schema já estão disponíveis. Os fluxos abaixo ainda precisam ser implementados nos casos de uso, adapters e workers.

### Persistência e transações

Usar `pgx/v5` com SQL explícito. O caso de uso controla uma única transação SQL e fornece o mesmo `pgx.Tx` aos repositórios envolvidos. Os repositórios não abrem commits independentes.

Uma operação confirma, no mesmo commit, resultado da transação financeira, alteração da carteira, ledger, outbox e inbox quando a origem for SQS. Rejeições de negócio também persistem resultado e evento. Erros transitórios de I/O provocam rollback e permitem nova tentativa.

A abertura positiva registra `OPENING`, ledger e dois eventos no commit da carteira, preservando versão `1`. Abertura com zero não cria movimento. Cada alteração posterior de saldo incrementa a versão; `LOSS` não altera saldo, ledger ou versão.

### Concorrência

Bloquear a carteira com `SELECT ... FOR UPDATE` antes de validar e alterar seu saldo. Usar uma ordem consistente na aquisição de locks. Carteiras diferentes não compartilham um lock global.

O schema já protege unicidade, não negatividade, aritmética e histórico. Os repositórios deverão preservar a ordem de locks e usar o resultado de commit para confirmar a operação. Instantes de atualização e lançamento serão definidos após adquirir o lock. A reconciliação será uma verificação independente da correspondência entre histórico e saldo.

### Idempotência

Persistir `(provider_id, idempotency_key)` e `(provider_id, external_transaction_id)` como identidades únicas. Um conflito de inserção deve ser tratado antes de tentar movimentar dinheiro.

Calcular SHA-256 sobre JSON dos campos de negócio com chaves ordenadas, incluindo moeda e valor decimal canônico. Excluir chave de idempotência, identidade da mensagem e metadados de transporte. HTTP e SQS usarão a mesma representação.

Guardar o saldo retornado pela execução original. Mesma chave e payload devolvem esse resultado; mudança de payload ou uso de outra chave para a mesma identidade financeira devolvem conflito.

### Estados e referências

O domínio já permite `PENDING` para `PROCESSED`, `PENDING_REFERENCE`, `REJECTED` ou `FAILED`. `PENDING_REFERENCE` permite novas tentativas e conclusão terminal. O schema protege os estados persistidos e exige agendamento quando houver espera por referência.

Operações sem dependências terminam na própria transação de entrada, sem commit intermediário de `PENDING`. Referências ausentes ou ainda pendentes geram `PENDING_REFERENCE` com retomada persistente, backoff e expiração. Referências que terminaram sem sucesso geram rejeição definitiva.

Os limites de retry e o critério de falha permanente serão definidos com os workers. Indisponibilidade temporária de PostgreSQL ou SQS não será classificada como rejeição de negócio.

### Reversões

Adotar uma política conservadora: uma `BET` aceita um `REFUND` ou um `ROLLBACK` bem-sucedido. Uma referência não admite uma segunda reversão bem-sucedida, inclusive de outro tipo. `ROLLBACK` de `REFUND` pode desfazer seu crédito, com saldo suficiente. A reversão do reembolso não reabre a aposta para outro reembolso.

Validar provedor, jogador, carteira, moeda, rodada e valor integral da referência. Débitos de reversão sem saldo serão rejeitados com um código diferente de `INSUFFICIENT_FUNDS`.

### Inbox e outbox

A inbox usará `(consumer_name, message_id)` do envelope e verificará o hash em reentregas. A mensagem SQS só será removida após commit. Pendências de referência poderão concluir a inbox quando sua continuação estiver persistida.

A outbox guardará snapshots tipados e imutáveis dos eventos com `eventId` estável. Publishers reservarão lotes com `FOR UPDATE SKIP LOCKED` e leases com expiração. A publicação ocorre fora da transação de reserva. Uma confirmação exige a mesma identidade de lease. Republicações preservam o identificador do evento.

### Identidade e autorização

Provisionar Keycloak com clientes de serviço `provider-a`, `provider-b` e `internal-service`, usando `client_credentials`. Validar assinatura, issuer, audience, expiração e permissões. A identidade validada define o provedor autorizado. Carteiras e reconciliação serão restritas ao serviço interno.

Controlar o acesso SQS por credenciais e políticas do broker. O consumidor preservará as mesmas validações financeiras do HTTP. Endpoints financeiros só serão expostos com autorização implementada.

### Reconciliação e observabilidade

Calcular o saldo por créditos menos débitos do ledger em uma visão consistente, incluindo `OPENING`. Retornar a diferença entre saldo armazenado e calculado, sem corrigir dados.

Acrescentar métricas de resultados, duplicatas, retries, DLQ, concorrência, latência, atraso da outbox e divergências. Os workers deverão interromper a busca de trabalho, concluir ou liberar reservas e terminar antes do fechamento das dependências.

## Referências

- [Ciclo de vida do Fx](https://uber-go.github.io/fx/lifecycle.html)
- [Pool PostgreSQL com pgx](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool)
