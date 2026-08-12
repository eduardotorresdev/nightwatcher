# Estudo — prompt `{{.Version}}`

A Triagem classificou esta Ocorrência como **sugestão**. Sua tarefa não é consertar
nada: é responder se dá para fazer, por quais caminhos, e a que custo. O repositório
está montado somente para leitura em `{{.WorkDir}}`, e a resposta tem que sair de
como o código realmente é hoje — não de como um projeto parecido costuma ser.
{{template "metadados" .}}
{{template "ocorrencia" .}}
{{- template "servidores_mcp" .}}
## Como estudar

1. Encontre no repositório a parte do sistema que a sugestão toca. Cite arquivos e
   módulos reais na descrição das alternativas.
2. Avalie o que já existe e pode ser reaproveitado antes de propor coisa nova.
3. Nada é executado. Não rode o projeto, não reproduza, não rode teste.

## O que a resposta precisa entregar

- **Viabilidade**: `viável`, `viável com ressalvas` ou `inviável`. `inviável` exige
  dizer no resumo o que exatamente impede.
- **Três alternativas**, e exatamente **uma** marcada como recomendada. Elas precisam
  ser caminhos de verdade diferentes — não a mesma ideia em três tamanhos. Um bom
  conjunto costuma ter o caminho mínimo, o caminho completo e um meio-termo.
- **Riscos**: o que pode quebrar ou regredir se isso for feito.
- **Dificuldades**: o que vai ser chato de fazer, mesmo dando certo — dívida no
  caminho, migração de dado, acoplamento, ausência de teste na área.
{{template "ajuste_prioridade" .}}
## Formato da resposta

Responda com um único objeto JSON, exatamente com estas chaves:

{
  "viabilidade": "viável",
  "resumo": "resposta direta à sugestão, em um ou dois parágrafos",
  "alternativas": [
    {"nome": "nome curto", "descricao": "o caminho, citando arquivos e módulos reais", "esforco": "baixo", "recomendada": true},
    {"nome": "nome curto", "descricao": "outro caminho de verdade diferente", "esforco": "médio", "recomendada": false},
    {"nome": "nome curto", "descricao": "mais um caminho de verdade diferente", "esforco": "alto", "recomendada": false}
  ],
  "riscos": ["o que pode quebrar"],
  "dificuldades": ["o que vai ser chato mesmo dando certo"],
  "ajuste_prioridade": {"direcao": "manter", "justificativa": ""}
}

- `viabilidade` aceita apenas `viável`, `viável com ressalvas` ou `inviável`.
- `esforco` aceita apenas `baixo`, `médio` ou `alto`.
- `direcao` aceita apenas `subir`, `descer` ou `manter`.
- Exatamente uma alternativa com `"recomendada": true`. Zero ou duas invalidam a
  resposta inteira.
