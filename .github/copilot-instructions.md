# Instruções para o Agente — POC Connection Pooling Proxy

## Idioma
Responda sempre em **português brasileiro**.

## Antes de qualquer implementação

Leia estes arquivos na ordem indicada antes de escrever código:

1. **PHASE-STATUS.md** — estado atual de cada fase, o que está feito, o que falta
2. **ARCHITECTURE.md** — topologia, fluxo end-to-end, componentes, config
3. **DECISIONS.md** — decisões técnicas já tomadas (não refazer/questionar sem motivo)
4. **CONTRACTS.md** — structs, funções públicas, assinaturas (consultar antes de abrir .go)

## Regras de execução

- Ao concluir uma fase, **atualize o PHASE-STATUS.md** com o estado novo.
- Ao tomar uma decisão técnica relevante, **registre em DECISIONS.md** como novo ADR.
- Não sugira refatorar algo que já tem ADR sem que o usuário peça.
- Use as assinaturas do CONTRACTS.md antes de abrir arquivos .go — só abra se precisar do corpo da função.
- O plano detalhado de cada fase está em `02-PLANO-DE-EXECUCAO.md`.

## Testes obrigatórios ao concluir cada fase

**Nunca considere uma fase concluída apenas com `go build`.**
Ao finalizar a implementação de uma fase, execute **todos** os testes abaixo antes de declarar a fase como concluída:

1. **Build** — `go build ./...` deve compilar sem erros
2. **Infraestrutura** — `docker compose up -d --build` deve subir todos os containers
3. **Smoke test** — conectar via `sqlcmd` (ou driver TDS) através do proxy e executar ao menos:
   - `SELECT 1` (conectividade básica)
   - `INSERT` + `SELECT` (operações de dados)
   - `BEGIN TRAN ... COMMIT` (transação)
4. **Testes específicos da fase** — validar os entregáveis da fase atual:
   - Cada funcionalidade nova deve ser exercitada com teste real (não apenas build)
   - Cenários de erro devem ser testados (timeout, rejeição, falha de conexão)
   - Métricas novas devem ser verificadas via `/metrics`
5. **Health check** — confirmar que `/health/live` retorna healthy após os testes
6. **Logs** — verificar nos logs do proxy que o fluxo está correto (sem erros inesperados)

Se algum teste falhar, corrija antes de marcar a fase como concluída.
Registre a validação completa na seção "Validação feita" do PHASE-STATUS.md.
