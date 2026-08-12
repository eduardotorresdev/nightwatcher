# Fixtures do Sentry — capturadas do `acme` real

Material real, capturado de `acme/acme` em 2026-08-11 por
`scripts/capture-sentry-fixtures.sh` — mais a `feedback-por-event-alert.json`, que veio de outro
lugar: da entrega real chegando no endpoint em produção, em 2026-08-12, porque um `event_alert` não
tem endpoint REST equivalente para capturar. O PRD exige payloads **gravados do real, não inventados**, e
é isto.

## Como foram construídas

O webhook entrega o `serialized_group` **mais** `url`, `web_url` e `project_url`. A listagem da API
REST usa o mesmo serializer base, então a captura lê a listagem, acrescenta as três URLs, remove as
chaves que só o serializer *detalhado* tem (`activity`, `participants`, `seenBy`, `stats`, `tags`,
`userReportCount`, `firstRelease`, `lastRelease`) e embrulha no envelope da Integration Platform.

`installation.uuid` é um marcador: ele só existe na entrega real. Nada nos testes depende do valor,
só de ele ser constante.

## Anonimização

Dado de usuário foi removido antes de versionar, como o ticket exige:

| Original | Vira |
|---|---|
| nomes dos dois operadores que enviam feedback | `OPERADOR UM`, `OPERADOR DOIS` |
| os dois e-mails reais | `operador.um@exemplo.invalid`, `operador.dois@exemplo.invalid` |
| qualquer outro e-mail | `usuario@exemplo.invalid` |
| `cliente: <nome>` no texto do feedback | `cliente: CLIENTE ANONIMIZADO` |
| endereços IP | `0.0.0.0` |

A `feedback-por-event-alert.json` é bem mais rica em dado pessoal que as outras — a entrega do
`event_alert` traz o evento inteiro — e por isso levou o mesmo tratamento estendido: nome e id de
cliente, `tenant`, id de pedido, id de ambiente, geolocalização (cidade e estado) e o IPv6 do
usuário, todos substituídos por marcadores. O `installation.uuid` foi uniformizado com o das demais,
porque o conjunto representa **uma** instalação.

`almoxarifado` aparece nos caminhos e **não** é dado de usuário — é rota da aplicação
(`/(sistema)/almoxarifado/...`), e é justamente o que o Diagnóstico precisa enxergar.

## O que cada uma representa

| Arquivo | `action` | `substatus` | `issueCategory` | `level` | Serve para |
|---|---|---|---|---|---|
| `defeito-error-new.json` | `created` | `new` | `error` | `error` | Defeito com stack trace em `CronogramaGrade.svelte`. `level: error` → **3 — Medium**. |
| `defeito-error-new-2.json` | `created` | `new` | `error` | `error` | Segundo defeito, `filename: <anonymous>` — o caso em que o trace **não** aponta arquivo. |
| `defeito-error-volume.json` | `created` | `new` | `error` | `error` | Defeito de alto volume (`count` 585, `userCount` 133) — mas na criação isso vale ~1 por construção. |
| `ruido-error-instrumentacao.json` | `created` | `new` | `error` | `error` | `Error: POS-SAIDA`: throw deliberado de instrumentação. **Ruído**, e um caso difícil: é `error`, não feedback. |
| `sugestao-feedback.json` | `created` | `ongoing` | `feedback` | `info` | Pedido de recurso ("relatório de quantidade de materiais de pintura") → Estudo, **4 — Low**. |
| `defeito-feedback.json` | `created` | `ongoing` | `feedback` | `info` | Feedback que é **defeito** ("Desapareceu a barra de pesquisa") — feedback não implica sugestão. |
| `defeito-feedback-com-dados-de-cliente.json` | `created` | `ongoing` | `feedback` | `info` | Idem, e prova a anonimização (`cliente:` no corpo). |
| `http-client-nao-previsto.json` | `created` | `ongoing` | `http_client` | `info` | **Categoria que nenhuma decisão previu.** Ver abaixo. |
| `outage-created.json` | `created` | `new` | `outage` | `error` | ⚠️ **Única sintética.** O `acme` não tem uptime/cron monitor, então não há material real. Deve ser recusada e registrada. |
| `regressao-do-defeito-error-new.json` | `unresolved` | `regressed` | `error` | `error` | Reincidência da **mesma** Ocorrência de `defeito-error-new.json`. |
| `escalada-do-defeito-error-volume.json` | `unresolved` | `escalating` | `error` | `error` | Escalada de `defeito-error-volume.json`, com `userCount` 133 → piso **2 — High**. |
| `ongoing-deve-ser-ignorado.json` | `unresolved` | `ongoing` | `error` | `error` | Deve ser ignorada. |
| `feedback-por-event-alert.json` | `triggered` | — | `feedback` | `info` | ⚠️ **Outro recurso de webhook.** `Sentry-Hook-Resource: event_alert`. Ver abaixo. |
| `tags-environment-*.json` | — | — | — | — | Resposta de `GET /issues/{id}/tags/environment/`. O `environment` não vem no webhook **da porta `issue`**. |

### Sobre regressão, escalada e ongoing

Os três derivam de uma Ocorrência real cujo `substatus` foi trocado. Não é invenção: a pesquisa
verificou no código do Sentry que `substatus` é **exatamente** o que muda entre `issue.created` e
as variantes de `issue.unresolved` — o resto do objeto é o mesmo serializer. Capturar uma regressão
autêntica exigiria esperar uma acontecer no `acme`.

### A segunda porta: `feedback-por-event-alert.json`

Capturada da entrega real em 2026-08-12, na produção do `watcher`, com `Sentry-Hook-Resource:
event_alert`. É a única fixture que **não** é do recurso `issue`, e existe porque a medição derrubou
uma premissa do desenho:

- Feedback **sempre** vira issue no Sentry — cinco criadas durante a sessão, todas
  `issueCategory: feedback`.
- Feedback **nunca** dispara `issue.created`. Zero entregas com recurso `issue` e `action: created`
  numa janela de ~50 min, com o checkbox `Issue` da integration comprovadamente ligado (entregas
  `issue` com `action: resolved` chegaram normalmente na mesma janela).
- Feedback **chega** como `event_alert`, disparado por Alert Rule, com corpo completo.

O `data.triggered_rule` da captura diz `Notify via autoissues`: o antecessor também nunca recebeu
`issue.created`, ele vivia de Alert Rule. Isso reconcilia o log dele citado no PRD com o que se
observa hoje.

O corpo tem outro formato — `data.event` em vez de `data.issue`, um **evento** do grupo em vez do
grupo — e cinco diferenças que importam:

| Campo | Nesta porta | Por que importa |
|---|---|---|
| identidade | `data.event.issue_id` | Mesmo id de grupo que `data.issue.id`. É o que permite reconhecer a mesma Ocorrência pelas duas portas. |
| `project` | **numérico** (`4509380076634112`) | Não casa com `sources.sentry_slug`. O slug da Fonte só existe dentro de `data.event.url`. |
| `substatus` | **não existe** | Por isso a dedup não pode depender só dele: ver "Par crítico de dedup". |
| `environment` | **vem no corpo** (`prod`) | Contradiz a premissa registrada antes. Nesta porta não custa a chamada de tags à parte. |
| categoria | `occurrence.type` = `6001` | Não há `issueCategory` algum. `6001` é FeedbackGroup; a ausência do bloco `occurrence` é um erro comum. |

O texto que o usuário escreveu está em `contexts.feedback.message`, verbatim — o `title` é o resumo
em inglês que o Sentry gerou. Uma Triagem que lesse o título estaria julgando uma paráfrase.

### A categoria `http_client`

A captura encontrou `issueCategory: "http_client"` (detecção de N+1), que **nenhuma decisão do mapa
previu**. O mapa de decisões manda recusar
`outage` e aceitar `error` e `feedback`, e ficou omisso sobre o resto. A consequência é que a
ingestão precisa de política para categoria **desconhecida** — recusar e registrar, nunca engolir —
em vez de uma lista de negação que só cobre `outage`.

## Par crítico de dedup

`defeito-error-new.json` + `regressao-do-defeito-error-new.json`: mesma Ocorrência, `substatus`
diferente. Tem que produzir **duas** Análises, não uma. Sem o `substatus` na chave, a regressão
seria engolida como entrega duplicada — o bug que a dedup existe para evitar, virado do avesso.

A **duplicata** de verdade é a mesma fixture entregue duas vezes. O que muda entre entregas é o
cabeçalho `Request-ID`, que por isso não serve como chave.

E o par que a segunda porta criou: a mesma Ocorrência entregue por `issue.created` **e** por
`event_alert`. Tem que produzir **uma** Análise, não duas — um erro que dispara a Alert Rule e cria
o grupo faz as duas coisas, e é o caso normal, não o excepcional. O `substatus` não distingue os
dois (o `event_alert` não traz nenhum), então a dedup tem uma segunda metade: **primeira aparição**
— `action` em (`created`, `triggered`) — que ignora o substatus de propósito. O `unresolved` fica de
fora dessa metade, que é o que mantém a regressão passando.

Não há duas fixtures reais do mesmo grupo pelas duas portas, e não pode haver: feedback **não**
dispara `issue.created`. O teste que cobre esse par constrói a entrega `issue` a partir da
`event_alert` gravada, e diz isso no comentário.

## Assinatura

Os arquivos guardam **só o corpo cru**. A assinatura é HMAC-SHA256 hex do Client Secret sobre
exatamente estes bytes; os testes a calculam com um segredo de teste, o que prova o caminho inteiro
em vez de gravar um digest morto.

Nunca faça `Unmarshal` → `Marshal` → HMAC: o `json.dumps` do Python e o `encoding/json` do Go
discordam em espaçamento e escaping, e a assinatura quebra.

## Repositório de fixture

`testdata/repo/` é um recorte com os arquivos que casam com os stack traces reais acima
(`CronogramaGrade.svelte`, `revisoes-ready-counter.svelte.ts`, a rota do terminal de separação e a
de acesso). Não é repositório git versionado; o teste roda `git init` sobre uma cópia em
`t.TempDir()`.
