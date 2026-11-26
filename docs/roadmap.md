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

## Depois
Mídia (imagens, áudio), grupos, figurinhas, busca, múltiplas contas.
