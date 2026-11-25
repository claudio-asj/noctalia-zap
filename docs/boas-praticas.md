# Boas práticas de plugin

Baseado na documentação oficial (manifest, plugin-api, workflow), no README do repositório da comunidade e nos
plugins oficiais.

## Estrutura do plugin

```
plugin/noctalia-zap/
  plugin.toml            manifesto (identidade, entradas, settings)
  service.luau           processo do daemon + estado
  bar.luau               widget da barra
  panel.luau             painel
  lib/                   módulos (style.luau, state.luau, protocol.luau, format.luau)
  translations/en.json   obrigatório (settings e textos da UI)
  translations/pt.json   português
  README.md              em inglês, vira a página do plugin
  thumbnail.webp         imagem do card na loja
  screenshots/           imagens usadas no README (caminhos relativos)
```

## Manifesto

- `id = "<autor>/noctalia-zap"`, `version` estrito (`1.0.0`, sem sufixo), `plugin_api` = o **menor** número
  que cobre o que usamos. Hoje: 56 (botões em notificação). Recursos ≥ 57 ainda não saíram em versão estável.
- `dependencies = ["noctalia-zapd"]` e o binário citado no README.
- `icon`, `description` (curta, em inglês) e `tags = ["communication", "panel"]`.

## Settings

- Declarar em `[[setting]]`. `label_key`/`description_key` são **chaves de tradução**, nunca texto literal.
- Ler só chaves declaradas (`noctalia.getConfig`). Chave não declarada gera aviso e retorna `nil`.
- Tipos úteis aqui: `bool` (privacidade, ocultar sem mensagens), `int` (limite de histórico), `file` (caminho do
  daemon) e `advanced = true` para opções de debug. `secret = true` para strings sensíveis.
- Reagir a mudanças em `onConfigChanged()`.

## Desempenho

- Cada entrada roda numa VM isolada com orçamento de tempo por chamada: nada de laços pesados.
- `setUpdateInterval` só quando há polling. Este plugin é orientado a eventos (`runStream`), então o intervalo
  fica alto ou nem é usado.
- `noctalia.state` copia os valores: guarde só o necessário (últimas N mensagens, não o histórico inteiro).
- Re-renderizar só quando o estado mudar (`state.watch`), não a cada tick.
- Prop de estilo omitida num novo render volta ao padrão: sempre passe o estilo completo.

## Ciclo de vida

- O service inicia o daemon no carregamento e trata `onExit(signal, reason)`, `onSuspend()` e `onResume()`
  (reconectar depois do suspend).
- O `runStream` morre no reload: o service tem que ser idempotente e conseguir subir de novo sem duplicar o daemon
  (`processMatches` antes de iniciar).

## Segurança

- Plugins rodam sem sandbox, com os poderes do usuário. Toda chamada de rede, escrita em disco e processo deve
  ser justificável e documentada no README.
- Não baixar nem executar código remoto.

## Antes de dar algo como pronto

1. `noctalia plugins lint plugin/noctalia-zap` sem avisos.
2. Nenhum erro do plugin em `~/.cache/noctalia/noctalia.log`.
3. Testado com dois temas (um claro e um escuro) e na barra horizontal.
4. Nenhuma string literal na UI e nenhum hex fixo (`git grep -nE '"#[0-9a-fA-F]{6}'`).
5. Textos revisados com `docs/escrita.md`.
