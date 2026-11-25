# Roadmap

## Fase 0: Base ✅
Estrutura, AGENTS.md (agnóstico de ferramenta), papéis de agentes, arquitetura, API, padrão visual, boas práticas e guia de escrita.

## Fase 1: Prova de conceito do daemon
- [ ] `go mod init` + whatsmeow; login por QR gerando PNG
- [ ] Sessão persistida no diretório de dados; reconexão automática
- [ ] Eventos JSON por linha: status, qr, chats, message
- [ ] Canal de comandos (socket unix + subcomando `ctl`)
- **Pronto quando:** logar com número secundário, receber e enviar texto pelo terminal

## Fase 2: Plugin mínimo
- [ ] Service inicia o daemon e popula o `noctalia.state`
- [ ] Widget com contador de não lidas
- [ ] Painel: tela de QR e lista de conversas

## Fase 3: Conversa
- [ ] Abrir conversa, histórico, campo de envio
- [ ] Marcar como lida; notificação com botão "Responder"

## Depois
Mídia (imagens, áudio), grupos, figurinhas, busca, múltiplas contas.
