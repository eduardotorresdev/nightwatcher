-- Servidor MCP remoto: o que a sessão longa pode consultar além do clone.
--
-- Só remoto. `stdio` não tem representação aqui de propósito: ele seria um processo
-- executando dentro do container da Análise, e "o nightwatcher só lê, nunca executa" é
-- estrutural, não uma intenção. Um transporte que não pode ser escrito não pode ser
-- configurado por engano.
--
-- A regra do 0001 vale inteira: NENHUMA coluna deste arquivo guarda credencial.
-- `mcp_headers.value_env` é o NOME de uma variável de ambiente; o valor vive só no
-- ambiente do daemon e entra no container por nome. O banco continua copiável.
CREATE TABLE mcp_servers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    -- NULL é global: vale para todo Projeto. NULL e não 0 porque a FK precisa
    -- continuar valendo para as linhas que apontam para um Projeto de verdade, e
    -- porque ON DELETE CASCADE só deve levar junto os do Projeto apagado.
    project_id INTEGER REFERENCES projects (id) ON DELETE CASCADE,

    -- Vira o prefixo mcp__<name>__ das ferramentas na sessão, e é por isso que o
    -- domínio o restringe a [a-z0-9_-].
    name        TEXT NOT NULL,
    transport   TEXT NOT NULL,
    url         TEXT NOT NULL,
    -- Para que o servidor serve, em uma linha. Chega ao prompt: modelo não usa
    -- ferramenta que não sabe existir.
    description TEXT NOT NULL DEFAULT '',

    created_at INTEGER NOT NULL
);

-- Dois índices parciais e não um UNIQUE (project_id, name), porque em SQLite NULL não
-- é igual a NULL: um índice único sobre a coluna anulável deixaria dois globais de
-- mesmo nome entrarem, e aí duas configurações disputariam o mesmo prefixo mcp__<name>__
-- na mesma sessão.
CREATE UNIQUE INDEX mcp_servers_global ON mcp_servers (name) WHERE project_id IS NULL;
CREATE UNIQUE INDEX mcp_servers_scoped ON mcp_servers (project_id, name) WHERE project_id IS NOT NULL;

-- Um header enviado ao servidor. O que vai na rede é prefix || getenv(value_env).
CREATE TABLE mcp_headers (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id INTEGER NOT NULL REFERENCES mcp_servers (id) ON DELETE CASCADE,
    name      TEXT    NOT NULL,
    -- Texto fixo na frente do segredo ("Bearer "). O domínio recusa qualquer coisa
    -- que pareça a credencial em si.
    prefix    TEXT    NOT NULL DEFAULT '',
    value_env TEXT    NOT NULL
);

CREATE INDEX mcp_headers_by_server ON mcp_headers (server_id);
