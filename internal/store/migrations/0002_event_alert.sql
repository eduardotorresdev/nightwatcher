-- A segunda porta de entrada de Ocorrência.
--
-- O desenho original assumiu que tudo chegava por `issue.created`. Medição em produção
-- em 2026-08-12 mostrou que feedback NUNCA dispara `issue.created` — ele chega como
-- `event_alert`, disparado por Alert Rule. As duas portas entregam o mesmo grupo em
-- formatos diferentes, e é isso que este índice protege.
--
-- `analyses_dedup` (0001) tem o substatus na chave, de propósito: sem ele uma regressão
-- de um grupo já visto seria engolida como reentrega. Mas um `event_alert` não traz
-- substatus algum, então ele entra como '' e NÃO colide com o `new` da mesma
-- Ocorrência entregue por `issue.created`. Sem este segundo índice, um erro que chega
-- pelas duas portas ao mesmo tempo — que é o caso normal, não o excepcional — vira duas
-- Análises, duas sessões do Claude e duas issues no Destino.
--
-- O predicado é por PRIMEIRA APARIÇÃO, não por grupo, e essa é a parte que importa:
-- `created` e `triggered` são as duas formas de o nightwatcher ouvir falar de um grupo
-- pela primeira vez. Um `unresolved` posterior — regressão, escalada — fica de fora e
-- continua podendo abrir Análise nova, que é exatamente o que a dedup exige.
--
-- Isto é a guarda de corrida; a verificação em `OccurrenceRecorded` é a que responde
-- "duplicada" com o id da Análise vencedora. As duas precisam existir: a consulta perde
-- a corrida quando duas entregas chegam juntas, e o índice é quem decide de fato.
CREATE UNIQUE INDEX analyses_first_sighting
    ON analyses (installation_uuid, sentry_project_id, sentry_issue_id)
    WHERE sentry_issue_id <> '' AND action IN ('created', 'triggered');
