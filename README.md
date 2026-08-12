# nightwatcher

Suporte autônomo: escuta o que os usuários relatam e o que quebra em produção,
decide o que é, investiga a causa lendo o código, e registra o resultado como
trabalho rastreável — sem intervenção humana no caminho.

Uma Ocorrência chega do Sentry por webhook. O nightwatcher a triga (defeito,
sugestão, dúvida ou ruído), e o que segue adiante vai para um agente que lê o
repositório — só lê, nunca executa — e devolve um Diagnóstico ou um Estudo. O
resultado nasce como issue no Linear, com prioridade derivada de fatos medidos da
Ocorrência e não de opinião.

O vocabulário do domínio (Projeto, Fonte Sentry, Destino, Ocorrência, Triagem,
Diagnóstico, Estudo, Prioridade, Análise) está definido em [CONTEXT.md](CONTEXT.md).
Vale ler antes do código: os nomes são contratuais e aparecem na linha de comando,
no SQLite e na resposta do agente.

## O binário é o daemon e a ferramenta de operação

Não há dois programas. `nightwatcher serve` sobe o daemon; todo o resto opera a
mesma instalação:

```
nightwatcher serve        # webhook do Sentry + worker de Análises
nightwatcher config       # credenciais globais e números da operação
nightwatcher project      # Projetos: repositório, Destino e Fontes Sentry
nightwatcher analysis     # o que o sistema andou fazendo
nightwatcher occurrence   # reprocessar e simular Ocorrências
```

Cada subcomando abre **direto** o SQLite que o daemon está usando
(`$NIGHTWATCHER_DB`, por padrão `/data/nightwatcher.db`). Não existe cliente HTTP
para o daemon. Em produção, portanto, operar é rodar o subcomando **dentro** do
mesmo container — é para isso que a imagem carrega o binário inteiro, e é por isso
que o `dokku run` aparece em todo lugar na documentação:

```
dokku run nightwatcher nightwatcher project list
```

**Consequência para quem baixa um binário publicado:** ele serve para compilar,
testar e rodar uma instância local. Ele **não** opera uma instância remota — sem o
arquivo SQLite daquela instância, não há o que ler nem o que escrever.

## Instalar

Binários prontos (linux e macOS, amd64 e arm64) saem em cada tag e estão em
[Releases](https://github.com/eduardotorresdev/nightwatcher/releases):

```
gh release download --pattern '*darwin_arm64*' -O nightwatcher.tar.gz
tar xzf nightwatcher.tar.gz && ./nightwatcher --version
```

Ou compilando:

```
go build -o nightwatcher ./cmd/nightwatcher
```

## Rodar local

```
export NIGHTWATCHER_DB=$PWD/nightwatcher.db
./nightwatcher config set ...     # credenciais e números
./nightwatcher project add        # guiado, validando cada resposta ao vivo
./nightwatcher serve              # escuta em $PORT (padrão 5000)
```

O `project add` guiado valida ao vivo contra o GitHub, o Sentry e o Linear, então
espera credenciais reais. `config validate` faz a mesma checagem isolada.

## Testes

```
go test ./...
```

Sem rede e sem Docker: todo serviço externo é servido por `httptest`, e o agente
tem um duplo em memória. `internal/tracker/spy.go` é o duplo do Destino contra o
qual a costura de integração afirma.

As fixtures em `testdata/sentry/` são capturas de entregas reais do Sentry, com os
identificadores do cliente substituídos. `testdata/sentry/INDEX.md` explica o que
cada uma exercita e por que os dois formatos de entrega (`issue` e `event_alert`)
descrevem a mesma Ocorrência.

## Deploy

A imagem carrega o binário Go **e** o Claude CLI, porque a decisão foi usar a
assinatura em vez da API por token. `serve` é o `CMD`; todo outro subcomando roda
por `dokku run` contra essa mesma imagem, compartilhando o volume persistente. O
daemon também sobe um container efêmero por Análise, e esse container é a mesma
imagem — daí o CLI e o binário viajarem juntos. Detalhes no [Dockerfile](Dockerfile).

## Licença

MIT. Ver [LICENSE](LICENSE).
