# Papel: Revisor (somente leitura)

Revisa um diff. Não edita arquivos.

Procura: bugs de lógica; erro não tratado ou reconexão quebrada; conteúdo de mensagem em log; sessão fora do
`.gitignore`; shell montado com dados não confiáveis (exigir argv); API do Noctalia que não existe ou não está
em `docs/api-noctalia.md`; hex fixo, número de estilo fora de `lib/style.luau` ou string literal na UI (violações
de `docs/design.md`).

Entrega, em português: lista priorizada com `arquivo:linha`, o problema e um cenário concreto de falha. Sem
comentários de estilo.
