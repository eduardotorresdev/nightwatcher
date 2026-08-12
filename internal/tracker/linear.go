package tracker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

const createIssueMutation = `
mutation CreateIssue($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue { id identifier url }
  }
}`

const createCommentMutation = `
mutation CreateComment($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
    comment { id url }
  }
}`

const createAttachmentMutation = `
mutation CreateAttachment($input: AttachmentCreateInput!) {
  attachmentCreate(input: $input) {
    success
    attachment { id url }
  }
}`

const createRelationMutation = `
mutation RelateIssues($issueId: String!, $relatedIssueId: String!) {
  issueRelationCreate(input: { issueId: $issueId, relatedIssueId: $relatedIssueId, type: related }) {
    success
    issueRelation { id type }
  }
}`

// A consulta de reincidência passa por `issues(... includeArchived: true)` e não pelo
// campo `issue(id:)` porque uma issue arquivada some das consultas por padrão. Sem
// isso o nightwatcher lê "nunca criei issue para esta Ocorrência" onde a verdade é
// "criei e arquivaram", e abre uma duplicata.
const issueStateByIDQuery = `
query IssueStateByID($id: ID!) {
  issues(filter: { id: { eq: $id } }, includeArchived: true, first: 1) {
    nodes { id identifier url archivedAt state { id name type } }
  }
}`

// O filtro por id só casa UUID. Para um identificador humano ("ACME-331") o caminho é
// o campo raiz, que aceita as duas formas.
const issueStateByKeyQuery = `
query IssueStateByKey($id: String!) {
  issue(id: $id) { id identifier url archivedAt state { id name type } }
}`

const teamByKeyQuery = `
query TeamByKey($key: String!) {
  teams(filter: { key: { eq: $key } }, first: 1) {
    nodes { id key name }
  }
}`

const teamByIDQuery = `
query TeamByID($teamId: String!) {
  team(id: $teamId) { id key name }
}`

// O Projeto guarda o Destino pelo nome que se lê no board ("acme"), mas o
// `projectId` do Linear é UUID. Mandar o nome cru faz a mutation inteira ser
// recusada com "Argument Validation Error" — sem dizer qual argumento.
const teamProjectsQuery = `
query TeamProjects($teamId: String!) {
  team(id: $teamId) {
    id
    projects(first: 250) { nodes { id name } }
  }
}`

const teamLabelsQuery = `
query TeamLabels($teamId: String!) {
  team(id: $teamId) {
    id
    labels(first: 250) { nodes { id name } }
  }
}`

const createLabelMutation = `
mutation CreateLabel($name: String!, $teamId: String!) {
  issueLabelCreate(input: { name: $name, teamId: $teamId }) {
    success
    issueLabel { id name }
  }
}`

type issueNode struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
	ArchivedAt string `json:"archivedAt"`
	State      struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
}

// CreateIssue faz nascer a issue no Destino: o Project do Projeto, a Prioridade já
// calculada, o corpo montado a partir do desfecho da Análise e o rótulo da Fonte.
func (c *Client) CreateIssue(ctx context.Context, req domain.IssueRequest) (domain.IssueRef, error) {
	teamID, err := c.resolveTeam(ctx)
	if err != nil {
		return domain.IssueRef{}, err
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Ocorrência sem título"
	}

	input := map[string]any{
		"teamId":      teamID,
		"title":       title,
		"description": req.Body,
		// A escala do Linear é invertida e o 0 fica fora da ordem, então o valor
		// atravessa inteiro: qualquer conta aqui produz o oposto do que se lê.
		"priority": int(req.Priority),
	}
	if id := c.resolveProject(ctx, teamID, req.LinearProjectID); id != "" {
		input["projectId"] = id
	}
	if ids := c.resolveLabels(ctx, teamID, req.Labels); len(ids) > 0 {
		input["labelIds"] = ids
	}
	if c.createAsUser != "" {
		input["createAsUser"] = c.createAsUser
	}
	// UUID fornecido pelo cliente: se a chamada for repetida por cota ou queda de
	// rede, a repetição carrega o mesmo id e não nasce uma segunda issue.
	if id, err := newUUIDv4(); err == nil {
		input["id"] = id
	}

	var out struct {
		IssueCreate struct {
			Success bool      `json:"success"`
			Issue   issueNode `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := c.do(ctx, "issueCreate", createIssueMutation, map[string]any{"input": input}, &out); err != nil {
		return domain.IssueRef{}, err
	}
	if !out.IssueCreate.Success || out.IssueCreate.Issue.ID == "" {
		return domain.IssueRef{}, fmt.Errorf("tracker: o Linear recusou a criação da issue %q sem apontar erro", title)
	}

	ref := domain.IssueRef{
		ID:         out.IssueCreate.Issue.ID,
		Identifier: out.IssueCreate.Issue.Identifier,
		URL:        out.IssueCreate.Issue.URL,
	}

	// Relação e anexo são enfeites da issue, não a issue. Falhar neles e devolver
	// erro faria o worker retentar e criar uma segunda issue idêntica — o oposto do
	// que se quer. Ficam registrados no log e a issue segue viva.
	if req.RelatedTo != "" {
		if err := c.relate(ctx, ref.ID, req.RelatedTo); err != nil {
			c.log.Warn("não consegui relacionar a issue à anterior",
				"issue", ref.Identifier, "anterior", req.RelatedTo, "erro", err)
		}
	}
	if req.AttachmentURL != "" {
		if err := c.attach(ctx, ref.ID, req.AttachmentURL, req.AttachmentTitle); err != nil {
			c.log.Warn("não consegui anexar a Ocorrência à issue",
				"issue", ref.Identifier, "url", req.AttachmentURL, "erro", err)
		}
	}
	return ref, nil
}

// Comment registra os dados novos na issue que já existe.
func (c *Client) Comment(ctx context.Context, issueID, body string) error {
	if strings.TrimSpace(issueID) == "" {
		return errors.New("tracker: comentário sem issue de destino")
	}
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	input := map[string]any{"issueId": issueID, "body": body}
	// Mesma chave de idempotência do issueCreate, e pelo mesmo motivo: `do` repete a
	// chamada quando a cota estoura ou a conexão cai, e uma queda DEPOIS de o Linear
	// gravar faria o mesmo comentário nascer duas vezes na issue. commentCreate
	// aceita `id` fornecido pelo cliente.
	if id, err := newUUIDv4(); err == nil {
		input["id"] = id
	}
	if c.createAsUser != "" {
		input["createAsUser"] = c.createAsUser
	}
	if err := c.do(ctx, "commentCreate", createCommentMutation, map[string]any{"input": input}, &out); err != nil {
		return err
	}
	if !out.CommentCreate.Success {
		return fmt.Errorf("tracker: o Linear recusou o comentário na issue %s sem apontar erro", issueID)
	}
	return nil
}

// IssueState responde se a issue anterior daquela Ocorrência continua aberta.
func (c *Client) IssueState(ctx context.Context, issueID string) (domain.IssueState, error) {
	if strings.TrimSpace(issueID) == "" {
		return domain.IssueState{}, nil
	}

	if isUUID(issueID) {
		var out struct {
			Issues struct {
				Nodes []issueNode `json:"nodes"`
			} `json:"issues"`
		}
		if err := c.do(ctx, "issues", issueStateByIDQuery, map[string]any{"id": issueID}, &out); err != nil {
			return domain.IssueState{}, err
		}
		if len(out.Issues.Nodes) > 0 {
			return stateOf(out.Issues.Nodes[0]), nil
		}
	}

	var out struct {
		Issue *issueNode `json:"issue"`
	}
	err := c.do(ctx, "issue", issueStateByKeyQuery, map[string]any{"id": issueID}, &out)
	if err != nil {
		// "Não existe" é resposta, não falha — mas é também o caminho que produz
		// issue duplicada quando confundido com erro, então só um erro que diga
		// literalmente "não encontrei" vira Exists: false.
		if isNotFound(err) {
			return domain.IssueState{Exists: false}, nil
		}
		return domain.IssueState{}, err
	}
	if out.Issue == nil {
		return domain.IssueState{Exists: false}, nil
	}
	return stateOf(*out.Issue), nil
}

// Validate prova que a credencial funciona e que o time existe, para o
// `nightwatcher config validate` falar de coisa alcançada e não de formato de token.
//
// A leitura acontece SEMPRE, mesmo com o UUID do time já configurado. resolveTeam
// devolve o TeamID sem tocar na rede, então parar por ali aprovaria uma chave
// revogada em silêncio — que é exatamente o formato de validação que este comando
// existe para não ser.
func (c *Client) Validate(ctx context.Context) error {
	teamID, err := c.resolveTeam(ctx)
	if err != nil {
		return err
	}
	var out struct {
		Team struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"team"`
	}
	if err := c.do(ctx, "team", teamByIDQuery, map[string]any{"teamId": teamID}, &out); err != nil {
		return err
	}
	if out.Team.ID == "" {
		return fmt.Errorf("tracker: a credencial não alcança o time %q do Destino", teamID)
	}
	return nil
}

func stateOf(n issueNode) domain.IssueState {
	return domain.IssueState{
		Exists:     true,
		Open:       isOpenStateType(n.State.Type),
		StateType:  n.State.Type,
		Identifier: n.Identifier,
		URL:        n.URL,
	}
}

// isOpenStateType decide aberto/fechado pelo TIPO do estado, nunca pelo nome.
//
// Os estados do time são customizados e em português — "Em produção", "Banco de
// tarefas" —, então comparar nomes classifica errado em silêncio. O tipo é fixo,
// em inglês, e as categorias terminais são exatamente três. Qualquer outro tipo é,
// por definição do produto, trabalho não terminado.
func isOpenStateType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "completed", "canceled", "duplicate":
		return false
	}
	return true
}

func isNotFound(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == http.StatusNotFound {
		return true
	}
	// O código exato de entidade ausente não está documentado na pesquisa, então a
	// checagem cobre o código e a frase que o Linear devolve na prática
	// ("Entity not found: Issue").
	//
	// A frase é casada inteira de propósito. Qualquer erro que apenas contenha
	// "not found" — um campo escrito errado na query, um argumento que o schema não
	// tem — viraria "essa issue nunca existiu", e o caminho de reincidência abriria
	// uma issue duplicada em silêncio. Errar para o outro lado só faz o worker
	// retentar, que é o erro barato.
	code := strings.ToUpper(apiErr.Code)
	if strings.Contains(code, "NOT_FOUND") || code == codeNotFound {
		return true
	}
	return strings.Contains(strings.ToLower(apiErr.Message), "entity not found")
}

func (c *Client) attach(ctx context.Context, issueID, url, title string) error {
	if title == "" {
		title = "Ocorrência no Sentry"
	}
	// attachmentCreate é idempotente pela url: recriar com a mesma url na mesma
	// issue atualiza o anexo em vez de duplicá-lo.
	input := map[string]any{"issueId": issueID, "url": url, "title": title}
	var out struct {
		AttachmentCreate struct {
			Success bool `json:"success"`
		} `json:"attachmentCreate"`
	}
	if err := c.do(ctx, "attachmentCreate", createAttachmentMutation, map[string]any{"input": input}, &out); err != nil {
		return err
	}
	if !out.AttachmentCreate.Success {
		return errors.New("o Linear recusou o anexo sem apontar erro")
	}
	return nil
}

func (c *Client) relate(ctx context.Context, issueID, relatedTo string) error {
	// `related` e não `similar`: `similar` existe no enum mas não aparece em nenhuma
	// página de produto, e relação que a UI não mostra é relação que ninguém lê.
	var out struct {
		IssueRelationCreate struct {
			Success bool `json:"success"`
		} `json:"issueRelationCreate"`
	}
	vars := map[string]any{"issueId": issueID, "relatedIssueId": relatedTo}
	if err := c.do(ctx, "issueRelationCreate", createRelationMutation, vars, &out); err != nil {
		return err
	}
	if !out.IssueRelationCreate.Success {
		return errors.New("o Linear recusou a relação sem apontar erro")
	}
	return nil
}

func (c *Client) resolveTeam(ctx context.Context) (string, error) {
	c.teamMu.Lock()
	defer c.teamMu.Unlock()
	if c.teamID != "" {
		return c.teamID, nil
	}

	var out struct {
		Teams struct {
			Nodes []struct {
				ID  string `json:"id"`
				Key string `json:"key"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := c.do(ctx, "teams", teamByKeyQuery, map[string]any{"key": c.teamKey}, &out); err != nil {
		return "", err
	}
	if len(out.Teams.Nodes) == 0 {
		return "", fmt.Errorf("tracker: não existe time com a chave %q no workspace", c.teamKey)
	}
	c.teamID = out.Teams.Nodes[0].ID
	return c.teamID, nil
}

// resolveProject traduz o Destino do Projeto — que é guardado pelo nome — no UUID
// que o Linear exige.
//
// Não devolve erro pelo mesmo motivo de resolveLabels: uma issue que nasce fora do
// Project custa um arrasto no board, enquanto abortar custaria a Ocorrência inteira.
func (c *Client) resolveProject(ctx context.Context, teamID, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if isUUID(name) {
		return name
	}

	c.projectMu.Lock()
	defer c.projectMu.Unlock()

	if !c.projectsLoaded {
		var out struct {
			Team struct {
				Projects struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"projects"`
			} `json:"team"`
		}
		if err := c.do(ctx, "team.projects", teamProjectsQuery, map[string]any{"teamId": teamID}, &out); err != nil {
			c.log.Warn("não consegui ler os Projects do time; a issue nasce fora do Project",
				"destino", name, "erro", err)
			return ""
		}
		for _, n := range out.Team.Projects.Nodes {
			// O nome no board pode divergir do configurado só na caixa ("acme" e
			// "acmE" são o mesmo Destino para quem escreveu a configuração).
			c.projectsByName[strings.ToLower(strings.TrimSpace(n.Name))] = n.ID
		}
		c.projectsLoaded = true
	}

	id, ok := c.projectsByName[strings.ToLower(name)]
	if !ok {
		c.log.Warn("não existe Project com esse nome no time; a issue nasce fora do Project",
			"destino", name)
		return ""
	}
	return id
}

// resolveLabels traduz os rótulos da Fonte em UUIDs, criando os que faltam.
//
// Nunca devolve erro: um rótulo que não pôde ser resolvido custa um filtro no board,
// enquanto abortar a criação custaria a Ocorrência inteira.
func (c *Client) resolveLabels(ctx context.Context, teamID string, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	c.labelMu.Lock()
	defer c.labelMu.Unlock()

	if !c.labelsLoaded {
		if err := c.loadLabels(ctx, teamID); err != nil {
			c.log.Warn("não consegui ler os rótulos do time; a issue nasce sem rótulo", "erro", err)
			return nil
		}
	}

	var ids []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		// A comparação é case-insensitive de propósito: criar "API" ao lado de uma
		// "api" que já existe parte o filtro do board em dois.
		key := strings.ToLower(name)
		if id, ok := c.labelsByName[key]; ok {
			ids = append(ids, id)
			continue
		}
		id, err := c.createLabel(ctx, teamID, name)
		if err != nil {
			c.log.Warn("não consegui criar o rótulo da Fonte; a issue nasce sem ele",
				"rótulo", name, "erro", err)
			continue
		}
		c.labelsByName[key] = id
		ids = append(ids, id)
	}
	return ids
}

func (c *Client) loadLabels(ctx context.Context, teamID string) error {
	var out struct {
		Team struct {
			Labels struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"labels"`
		} `json:"team"`
	}
	if err := c.do(ctx, "team.labels", teamLabelsQuery, map[string]any{"teamId": teamID}, &out); err != nil {
		return err
	}
	for _, l := range out.Team.Labels.Nodes {
		c.labelsByName[strings.ToLower(l.Name)] = l.ID
	}
	c.labelsLoaded = true
	return nil
}

func (c *Client) createLabel(ctx context.Context, teamID, name string) (string, error) {
	var out struct {
		IssueLabelCreate struct {
			Success bool `json:"success"`
			Label   struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"issueLabel"`
		} `json:"issueLabelCreate"`
	}
	vars := map[string]any{"name": name, "teamId": teamID}
	if err := c.do(ctx, "issueLabelCreate", createLabelMutation, vars, &out); err != nil {
		return "", err
	}
	if !out.IssueLabelCreate.Success || out.IssueLabelCreate.Label.ID == "" {
		return "", fmt.Errorf("o Linear recusou o rótulo %q sem apontar erro", name)
	}
	return out.IssueLabelCreate.Label.ID, nil
}

// newUUIDv4 existe porque o pacote não pode ganhar dependência nova e o único uso de
// UUID aqui é a chave de idempotência do issueCreate.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
