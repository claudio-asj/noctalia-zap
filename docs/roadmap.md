# Roadmap

## Fase 0: Base ✅
Estrutura, AGENTS.md (agnóstico de ferramenta), papéis de agentes, arquitetura, API, padrão visual, boas práticas e guia de escrita.

## Fase 1: Prova de conceito do daemon
- [x] `go mod init` + whatsmeow; login por QR gerando PNG
- [x] Sessão persistida no diretório de dados; reconexão automática
- [x] Eventos JSON por linha: status, qr, chats, message
- [x] Canal de comandos (socket unix + subcomando `ctl`)
- **Pronto quando:** logar com número secundário, receber e enviar texto pelo terminal ✅ (testado pelo painel em 2026-11-26)

## Fase 2: Plugin mínimo
- [x] Service inicia o daemon e popula o `noctalia.state`
- [x] Widget com contador de não lidas
- [x] Painel: tela de QR e lista de conversas

## Fase 3: Conversa
- [x] Abrir conversa, histórico, campo de envio
- [x] Marcar como lida; notificação com botão "Responder"
- **Pronto quando:** abrir conversa, enviar e receber notificação pelo painel ✅ (testado em 2026-11-26)

## Fase 4: Mídia recebida
- [x] Miniatura de foto e vídeo na hora (prévia que vem dentro da mensagem)
- [x] `ctl media`: baixar e descriptografar para o cache em `pluginDataDir`; clique abre no app padrão
- [x] Áudio e voz com tocar/parar (o daemon controla o `mpv`) e duração
- [x] Documentos e figurinhas: baixar e abrir
- [x] Cache com limite configurável e limpeza automática
- [x] Pasta de downloads configurável (padrão Downloads/WhatsApp); áudio só no cache
- [x] Recuperar mídia antiga ao abrir a conversa (histórico sob demanda + miniaturas)
- **Pronto quando:** ver miniatura, abrir foto em tamanho real e ouvir um áudio pelo painel ✅ (testado em 2026-11-26)

## Fase 5: Contatos e fotos
- [x] Nomes para as conversas que só mostravam telefone (contato, nome de perfil, última mensagem)
- [x] Conversas vazias do history sync escondidas
- [x] Foto de perfil no avatar, buscada ao abrir a conversa (cache, no máximo 1×/dia)
- [x] Indicador "Atualizando…" no cabeçalho enquanto busca foto e mídias antigas
- [x] Nome colorido de quem escreveu nos grupos
- **Pronto quando:** nomes e fotos certos no painel ✅ (testado em 2026-11-26)

## Fase 6: Conversa completa
- [ ] Respostas citadas e responder a uma mensagem
- [ ] Reações; mensagens editadas e apagadas
- [ ] Status de entrega (enviado, entregue, lido) e "digitando…"
- [ ] Carregar mensagens antigas ao rolar para cima

## Fase 7: Enviar mídia
- [ ] Colar imagem da área de transferência
- [ ] Anexar arquivo (portal ou zenity, a confirmar)
- [ ] Gravar áudio (`pw-record`)

## Fase 8: Busca e atalhos
- [ ] Busca no texto das mensagens (SQLite FTS5, local)
- [ ] Atalhos: Ctrl+F, Esc, setas
- [ ] Arquivar, silenciar e fixar pelo painel

## Fase 9: Publicação
- [ ] README com capturas, thumbnail e descrição
- [ ] Binário do daemon nos releases e pacote no AUR
- [ ] Envio para a loja de plugins da comunidade

## Fora de escopo
Múltiplas contas (custo alto, pouco uso) e chamadas (o whatsmeow não suporta).
