# Papéis de agentes

Divisão de trabalho que vale para qualquer ferramenta. O **agente principal** (o modelo mais capaz) planeja,
decide a arquitetura, integra e confere. As tarefas fechadas vão para modelos mais baratos.

| Papel | Nível de modelo | Exemplos de modelo | Arquivo |
|---|---|---|---|
| Explorador | rápido/barato | Claude Haiku, GPT mini, Gemini Flash | `explorador.md` |
| Dev Go | intermediário | Claude Sonnet, GPT padrão, Gemini Pro | `dev-go.md` |
| Dev Luau | intermediário | idem | `dev-luau.md` |
| Revisor | intermediário | idem | `revisor.md` |

## Como delegar

- Uma tarefa por agente, com escopo explícito: arquivos, objetivo e critério de pronto.
- Tarefas independentes em paralelo. Uma tarefa que depende de outra espera a anterior.
- Não delegue o que se resolve em 1 a 3 ações. Faça direto.
- Confira o resultado (build, teste, lint, leitura pontual). Não refaça o trabalho do agente.
- Todo agente começa lendo `AGENTS.md`, a fonte única de regras.

## Adaptadores por ferramenta

Os arquivos de cada ferramenta só apontam para cá. As instruções de verdade ficam neste diretório.

- Claude Code: `.claude/agents/*.md` (com o modelo definido no frontmatter).
- Outras ferramentas: passe o conteúdo do papel como instrução do subagente, ou siga o papel manualmente.
