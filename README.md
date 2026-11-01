# SilverBullet Relay

Backend Go para Readeck, conexões Vikunja, calendário opcional, backup Git e ponte Desktop do SilverBullet 2.11.1. Daily/Zotero são implementados exclusivamente no Paperwork; as rotas legadas são encaminhadas ao Paperwork em loopback, e o Relay não registra seus jobs.

A biblioteca usa Space Lua e fontes TypeScript em `src/Library/Scripts`. O JS em `Library/Scripts` é gerado para instalação nativa e marcado como artefato no GitHub. Não há backend Python.

Mantenha o checkout Paperwork ao lado deste checkout. A dependência pública Go usa esse checkout local durante desenvolvimento; Paperwork funciona sem um serviço Relay. O módulo privado Pressroom pode compor o mesmo host sem entrar no repositório público.

```sh
npm ci
npm run build
cd go
go test -p 1 ./...
go build -o ../bin/integration-service ./cmd/relay
```

Use `sb-tools` do Paperwork para gerar pacotes: `package --source CAMINHO_RELAY --kind relay --binary BINARIO --paperwork CAMINHO_PAPERWORK --paperwork-binary BINARIO_PAPERWORK --destination DESTINO_NOVO`, depois `bootstrap --package DESTINO_NOVO`. Configurações e credenciais ficam em runtime fora do Git. Novas instalações mantêm os jobs Daily/Zotero e publicação desativados até configuração explícita no serviço proprietário.
