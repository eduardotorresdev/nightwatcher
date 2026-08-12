# Contexto: nightwatcher

Suporte autônomo: escuta o que os usuários relatam e o que quebra em produção,
decide o que é, investiga a causa lendo o código, e registra o resultado como
trabalho rastreável — sem intervenção humana no caminho.

## Glossário

### Projeto
Unidade de configuração do nightwatcher. Agrega **um** repositório, **um**
Destino e **uma ou mais** Fontes Sentry. Um produto com frontend e backend
separados no Sentry é um único Projeto, não dois.

### Fonte Sentry
Um projeto no Sentry que alimenta um Projeto do nightwatcher. Carrega
características próprias (ex.: se é a camada web ou a API) que sobrevivem até o
destino final.

Essa característica tem nome: o **rótulo**. É texto livre, vira label no Linear
criada sob demanda, e é o que faz "me mostra tudo que veio da API" ser filtro no
board em vez de busca textual.

### Destino
Onde o resultado de uma Análise é registrado como trabalho: um time do Linear
combinado com um Project dentro dele. O time é o mesmo para todos os Projetos; o
Project é o que os distingue.

### Ocorrência
O que chega do Sentry e merece consideração: um grupo de erros ou um feedback
publicado por um usuário. É sempre o *grupo*, nunca a ocorrência individual — um
erro que acontece cinco mil vezes é uma Ocorrência, não cinco mil.

A mesma Ocorrência é entregue mais de uma vez pelo Sentry. Reconhecê-la como já
vista é obrigação do nightwatcher, não cortesia.

Ela chega por **duas portas**, e as duas descrevem o mesmo grupo em formatos
diferentes:

- **`issue`** — o grupo em si, quando ele é criado ou muda de estado. É a porta
  por onde erro chega.
- **`event_alert`** — um evento do grupo, entregue porque uma Alert Rule do
  Sentry disparou. É a **única** porta por onde feedback de usuário chega:
  feedback sempre vira issue no Sentry e nunca dispara `issue.created`, o que foi
  medido em produção em 2026-08-12.

Duas portas não são duas Ocorrências. Um erro que dispara a Alert Rule *e* cria o
grupo chega pelas duas, e continua sendo uma Ocorrência só — é a mesma obrigação
de reconhecer o repetido, com o agravante de que o formato do repetido é outro.

### Triagem
Primeiro julgamento sobre uma Ocorrência: o que ela é. Quatro desfechos
possíveis, e só os dois primeiros seguem adiante:

- **defeito** — algo está quebrado. Segue para Diagnóstico.
- **sugestão** — pedido de recurso ou melhoria. Segue para Estudo.
- **dúvida** — o usuário quer saber como usar. Encerra aqui.
- **ruído** — teste, mensagem vazia, spam, ininteligível. Encerra aqui.

Encerrar não é apagar: toda Ocorrência descartada continua consultável, para que
a qualidade da Triagem possa ser auditada depois.

### Diagnóstico
Investigação da causa de uma Ocorrência que a Triagem classificou como defeito.
Feita **apenas por leitura**: o código do repositório e os dados do Sentry. O
projeto nunca é executado, nada é reproduzido, nenhum teste roda.

### Estudo
O equivalente ao Diagnóstico para uma Ocorrência classificada como sugestão.
Também lê o código, mas responde outra pergunta: se dá para fazer, por quais
caminhos, e a que custo. Entrega viabilidade, três alternativas com uma
recomendada, riscos e dificuldades previstas.

### Servidor MCP
Uma fonte de conhecimento fora do repositório que o Diagnóstico e o Estudo podem
consultar: documentação de biblioteca, esquema de um serviço, histórico de um
board. Configurado por Projeto ou globalmente — o global vale para todos, o do
Projeto soma e ganha em colisão de nome.

Só **remoto** (`http` ou `sse`). `stdio` está fora e permanece fora: seria um
processo executando dentro do container da Análise, que é exatamente o que a
regra "só lê, nunca executa" proíbe.

A Triagem nunca fala com um servidor MCP: é uma sessão de um turno, sem clone e
sem ferramenta.

A credencial **nunca** entra no banco. O que fica gravado é o *nome* de uma
variável de ambiente; o valor vem do ambiente do daemon e entra no container por
nome, nunca em argv.

### Prioridade
A urgência com que uma issue nasce no Destino. Não é opinião: parte de fatos
mensuráveis da Ocorrência (quantas pessoas atingiu, com que frequência, em qual
ambiente, se é reincidência) e só então admite o julgamento do agente sobre
gravidade que número não captura — limitado a um nível para cima ou para baixo, e
sempre justificado.

### Rajada
Muitas Ocorrências novas do mesmo Projeto em pouco tempo, quase sempre sintomas
de uma causa única, tipicamente uma release ruim. Tratar cada uma como Ocorrência
independente produz dezenas de registros para um problema só. Reconhecer Rajadas
é objetivo do nightwatcher, mas não da primeira versão.

### Análise
O percurso completo de uma Ocorrência: da entrega pelo Sentry até o desfecho.
Tem começo, fim e resultado registrado — inclusive quando o desfecho é
"descartada" ou "falhou".

Quatro desfechos, todos de primeira classe e todos gravados:

- **publicada** — virou issue no Destino.
- **encerrada** — a Triagem disse dúvida ou ruído; nada foi criado.
- **recusada** — não chegou a entrar na fila: assinatura inválida, Fonte
  desconhecida, categoria `outage`, ou entrega duplicada.
- **falhou** — as retentativas se esgotaram; vira issue marcada como não
  analisada, com o motivo.

Gravar os quatro é o que torna a Triagem auditável e o que impede o defeito que
matou o antecessor: Ocorrência sumindo em silêncio.

## Antecessor

O `autoissues`/`sentrybot` cumpria papel parecido: rodava local, exposto por
túnel, triava com o Codex lendo só o Sentry, e abria issue no GitHub. O
nightwatcher o substitui. Diferenças que motivaram a substituição estão nos ADRs.

## Convenção de código

Identificadores em Go são em inglês, por convenção da linguagem. Os **valores** dos
enums do domínio permanecem em português porque são contratuais: aparecem na linha
de comando (`--outcome defeito|sugestão|dúvida|ruído`), nas linhas do SQLite e na
resposta que o agente devolve. Trocá-los quebra dados já gravados e a superfície
que o operador digita.
