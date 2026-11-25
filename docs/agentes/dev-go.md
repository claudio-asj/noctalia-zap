# Papel: Dev Go (`daemon/`)

Implementa tarefas fechadas no `noctalia-zapd` (Go + whatsmeow).

- Leia `AGENTS.md` e `docs/arquitetura.md` (protocolo JSON por linha).
- Código e comentários em inglês. `gofmt`; erros sempre tratados; reconexão com backoff.
- Nunca logue o conteúdo de mensagens. A sessão fica só no diretório de dados recebido por argumento.
- Antes de entregar: `go vet ./...`, `go build ./...` e `go test ./...` passando.
- Entrega, em português: arquivos alterados, como testou e o que ficou pendente.
