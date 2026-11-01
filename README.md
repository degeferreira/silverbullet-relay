# SilverBullet Relay

Integrações portáveis para SilverBullet 2.11.1: Daily, Vikunja, Zotero,
Readeck, citações, calendário opcional e conexão entre computadores.

## Instalação limpa

No checkout do Relay, execute:

```sh
python3 package.py /caminho/instalacao
python3 bootstrap.py /caminho/instalacao
```

Execute `configure.py` na pasta gerada para guardar os tokens fora do Space.
Inicie com `docker compose up -d --build` na pasta gerada. Acesse o SilverBullet
em `http://127.0.0.1:13000`, configure a autenticação e gere o token do Space.
Conectores começam desativados. Configure os destinos em **Relay: Configurações**
e reinicie somente `integration-service` para aplicar configurações e credenciais.
Não use a inicialização limpa sobre uma instalação existente.

As URLs das APIs devem ser acessíveis de dentro do contêiner. `127.0.0.1`
nesse contexto é o namespace do SilverBullet, não outros serviços do host.
Conecte as redes Docker necessárias ou use os endereços reais dos servidores.

## Migração existente

Não reinicialize o banco ou os tokens. A migração local preparada está em
`migration/silverbullet-extensions-20261031` no servidor de origem. Ela mantém
configuração e caminhos, preserva os comandos antigos e fornece o endereço
legado por configuração local. O pacote público não contém esse endereço.

O painel guarda alterações em `/state/configuration`; segredos nunca são
retornados pelo endpoint de leitura. Mudanças pendentes passam a valer no
próximo início do processo. Para mudar de instância/conta com tarefas mapeadas,
use um estado separado: IDs remotos pertencem à instância original.

## Estado de desenvolvimento

Os fluxos originais têm testes de regressão. O menu ainda requer validação
interativa no navegador do usuário. Google Calendar conserva o fluxo OAuth
existente; o assistente completo de autorização via menu está pendente.

O `Library/Manifest.md` permite instalar a interface pelo gerenciador nativo; ele não instala o backend. Para incluir Paperwork separado na porta 8089, mantenha seu checkout irmão com nome `paperwork` e use `package.py DESTINO --paperwork`.

Repositório: https://github.com/degeferreira/silverbullet-relay

## Testes

Instale as dependências do backend em ambiente virtual e execute:

```sh
PYTHONPATH=backend python -m unittest discover -s tests -q
```

## Desenvolvimento TypeScript

Os módulos de navegador têm fonte em `src/Library/Scripts/*.ts`, com verificação
`strict`. O build mantém os caminhos `Library/Scripts/*.js` usados pelo Space Lua
 e pelo empacotador. Esses arquivos JavaScript gerados permanecem no repositório
para instalação sem Node.js; edite a fonte TypeScript e reconstrua antes de empacotar.
Space Lua permanece nas páginas da Library.

```sh
npm ci
npm run check
npm run build
```

O compilador é fixado no `package-lock.json`. O build não instala bibliotecas nem
altera serviços em execução. Os testes existentes consomem o JavaScript gerado.

### Estado da implementação Go/TypeScript

Fontes Go e TypeScript em desenvolvimento; a presença destes fontes não constitui uma release pronta para substituir o serviço Python. A transferência exclusiva de Daily/Zotero, os consumidores do estado migrado e a instalação limpa ainda precisam ser concluídos antes do corte de produção. Os bancos e credenciais de produção não fazem parte deste repositório.
