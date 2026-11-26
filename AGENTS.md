# Noctalia Zap: instruções para agentes de IA

Este arquivo é a fonte única de instruções para **qualquer** agente de IA (Claude Code, Codex, Gemini CLI,
Cursor, Copilot, Aider...). `CLAUDE.md` e `GEMINI.md` são links simbólicos para cá. Não crie instruções
paralelas em arquivos específicos de uma ferramenta; edite este.

Projeto: cliente de WhatsApp como plugin do Noctalia Shell (niri/Wayland, Arch Linux). Projeto pessoal.
**Status: daemon da fase 1 implementado; falta testar com número real e começar o plugin.** Ver `docs/roadmap.md`.

## Mapa

| Caminho | Conteúdo |
|---|---|
| `daemon/` | `noctalia-zapd`: auxiliar em Go + `go.mau.fi/whatsmeow`, protocolo JSON por linha |
| `plugin/noctalia-zap/` | plugin Noctalia em Luau (service, widget de barra, painel). Toda a interface |
| `docs/arquitetura.md` | desenho do sistema e protocolo daemon ↔ plugin |
| `docs/api-noctalia.md` | API de plugins: o que existe, gates de `plugin_api` e limitações confirmadas |
| `docs/design.md` | **padrão visual**: papéis de cor, tamanhos, componentes, telas. Obrigatório para UI |
| `docs/boas-praticas.md` | regras de plugin (manifesto, settings, traduções, desempenho, publicação) |
| `docs/escrita.md` | como escrever textos para pessoas sem cara de IA |
| `docs/agentes/` | papéis para dividir o trabalho entre agentes mais baratos |
| `docs/roadmap.md` | fases e critérios de pronto |

## Regras invioláveis

1. Respostas ao usuário em **português (pt-BR)**. Código, identificadores, comentários, `plugin.toml`, `README.md`
   do plugin e `translations/en.json` em **inglês**.
2. UI só com **papéis de cor do tema** (`primary`, `on_surface`...), nunca hex fixo. Siga `docs/design.md`.
3. Nenhum texto literal na interface: tudo via `noctalia.tr("chave")` + `translations/en.json` (e `pt-BR.json`).
4. Persistir só em `noctalia.pluginDataDir()`, nunca em `pluginDir()` (é sobrescrito em updates).
5. **Nunca versionar a sessão do WhatsApp** (`*.db`, `data/`, `session/`) e nunca logar o conteúdo das mensagens.
6. Processos externos sempre com **argv** (`runAsync({...})`), nunca string de shell montada com dados de mensagem.
7. Antes de usar uma API do Noctalia, confira em `docs/api-noctalia.md`. Se faltar, procure nos exemplos (abaixo)
   e **atualize o doc** com o que confirmar.
8. Sem código ofuscado, gerado ou minificado. Dependências externas declaradas em `dependencies` no `plugin.toml`.

## Referências locais

- Plugins oficiais (padrão de qualidade): `~/.local/state/noctalia/plugins/sources/official/repo`. Comece pelo
  plugin `example/`.
- Plugins da comunidade: `~/.local/state/noctalia/plugins/sources/community/repo`.
- Os dois são repositórios git: leia com `git show HEAD:<plugin>/<arquivo>` e busque com `git grep <padrão> HEAD`.
- Log do shell: `~/.cache/noctalia/noctalia.log`. Lint: `noctalia plugins lint plugin/noctalia-zap`.
- Desenvolvimento: link `~/.local/share/noctalia/plugins/noctalia-zap -> plugin/noctalia-zap`. Os `.luau`
  recarregam sozinhos; mudanças no manifesto pedem `noctalia msg config-reload`.

## Divisão de trabalho (economia)

Se a sua ferramenta permite subagentes ou modelos diferentes, use os papéis de `docs/agentes/`: o agente
principal planeja e integra, e tarefas fechadas vão para modelos mais baratos. Se não permitir, siga os mesmos
papéis em sequência. Detalhes em `docs/agentes/README.md`.

## Textos para pessoas

README, descrição do plugin, textos da interface, mensagens de commit: siga `docs/escrita.md`. Se a sua ferramenta
tiver uma skill de "humanizer" ou equivalente, use-a no final.
