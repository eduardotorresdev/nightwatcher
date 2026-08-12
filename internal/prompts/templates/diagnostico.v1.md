# Diagnóstico — prompt `{{.Version}}`

A Triagem classificou esta Ocorrência como **defeito**. Sua tarefa é descobrir a
**causa provável** lendo o código do repositório, que está montado somente para
leitura em `{{.WorkDir}}`.

O antecessor deste sistema falhou exatamente aqui: ele lia só o Sentry e devolvia uma
descrição do sintoma, nunca o lugar no código. Um diagnóstico que não aponta arquivo
não vale nada — a investigação continua comigo, que é o trabalho que este sistema
existe para tirar de mim.
{{template "metadados" .}}
{{template "ocorrencia" .}}
## Como investigar

1. Comece pelo culprit e pelo stack trace: procure no repositório os arquivos e
   símbolos citados, com busca por nome e por conteúdo.
2. Abra os arquivos. Leia o caminho que leva ao ponto do erro, não só a linha dele.
3. Suba até a origem: o lugar onde o valor errado nasceu costuma estar acima do lugar
   onde a exceção estourou.
4. Se o stack trace apontar para dependência de terceiros, encontre a chamada **do
   projeto** que entrou nela.
5. Nada é executado. Não rode o projeto, não reproduza, não rode teste.

## Regras sobre os arquivos suspeitos

- Todo caminho em `arquivos_suspeitos` tem que **existir de verdade** no repositório
  montado, e você só cita um caminho depois de tê-lo aberto ou encontrado por busca.
  Caminho inventado é pior que nenhum caminho.
- Use caminho relativo à raiz do repositório (`src/services/pedido.ts`), nunca
  caminho absoluto.
- Pelo menos um arquivo é obrigatório. Se o código não permitir concluir, escolha o
  arquivo mais provável, diga na causa provável o que faltou para fechar e marque
  `confianca` como `baixa`.
- No máximo cinco arquivos, ordenados do mais provável para o menos provável.
- `linha` é opcional: use `0` quando não souber.
{{template "ajuste_prioridade" .}}
## Formato da resposta

Responda com um único objeto JSON, exatamente com estas chaves:

{
  "causa_provavel": "o que provavelmente causa o defeito, em uma ou dois parágrafos, citando os arquivos pelo nome",
  "arquivos_suspeitos": [
    {"caminho": "src/services/pedido.ts", "linha": 128, "porque": "o que neste arquivo liga ao erro"}
  ],
  "evidencias": ["trecho de código ou do stack trace que sustenta a conclusão"],
  "confianca": "alta",
  "ajuste_prioridade": {"direcao": "manter", "justificativa": ""}
}

- `confianca` aceita apenas `alta`, `média` ou `baixa`.
- `direcao` aceita apenas `subir`, `descer` ou `manter`.
- `evidencias` pode ser lista vazia, mas com evidência o diagnóstico vale mais.
