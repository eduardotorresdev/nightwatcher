// Package tracker records the outcome of an Análise as work in the Destino: an issue
// in the Projeto's Linear Project, or a comment on the issue that already exists.
//
// This is the boundary the PRD's Testing Decisions call "o mundo externo" — what the
// tests assert on. The package therefore ships two implementations of domain.Tracker:
// Client, which talks to the Linear GraphQL API, and Spy, the in-memory double the
// integration seam asserts against.
//
// Every fact encoded here about the Linear API comes from the Linear API docs. The
// ones that are load-bearing are commented where they are used, because
// each of them is a bug that would otherwise be silent: the inverted priority scale,
// the rate limit that arrives as HTTP 400, the personal key that must NOT carry the
// `Bearer` prefix, and openness decided by state.type instead of the state's name.
package tracker

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// Endpoints da API. Ficam em variáveis de pacote apenas para os testes apontarem
// para um httptest; nada em produção os troca.
const (
	DefaultEndpoint      = "https://api.linear.app/graphql"
	DefaultTokenEndpoint = "https://api.linear.app/oauth/token"

	// DefaultScopes é o par mínimo que cobre issueCreate, commentCreate,
	// attachmentCreate e issueRelationCreate. Não existe mapeamento oficial
	// scope→mutation, então escopos mais estreitos (`issues:create`) são um chute
	// que só falha em produção.
	DefaultScopes = "read,write"

	// DefaultCreateAsUser é a autoria que aparece na UI do Linear. Só vale em modo
	// actor=app; com chave pessoal o Linear recusa o campo, e por isso ele é
	// omitido nesse caso.
	DefaultCreateAsUser = "nightwatcher"
)

// Options configura o Client. Só as credenciais e o time são obrigatórios; o resto
// tem padrão de produção.
type Options struct {
	// Secrets traz as credenciais do Linear: o par client_id/client_secret da conta
	// de serviço, ou LinearAPIKey como alternativa de desenvolvimento.
	Secrets config.Secrets

	// TeamID é o UUID do time do Linear que é o Destino. O time é o mesmo para
	// todos os Projetos; o que os distingue é o Project, que vem em cada
	// IssueRequest. TeamKey ("ACME") é aceito no lugar e resolvido na primeira
	// chamada.
	//
	// NOTA: o time não está em internal/config nem em domain.Project — não
	// há LINEAR_TEAM_ID. Quem constrói o Client precisa passá-lo.
	TeamID  string
	TeamKey string

	Endpoint      string
	TokenEndpoint string
	Scopes        string

	// CreateAsUser é a autoria exibida na UI. Vazio usa DefaultCreateAsUser.
	CreateAsUser string

	HTTPClient *http.Client
	Clock      domain.Clock
	Logger     *slog.Logger

	// Retries é quantas vezes uma chamada é repetida quando o erro é retentável
	// (cota estourada, 5xx, falha de rede). MaxBackoff é a espera máxima aceita
	// dentro de uma chamada: acima disso o erro sobe com RetryAfter preenchido para
	// o worker reenfileirar, em vez de segurar uma sessão parada por uma hora.
	Retries     int
	MaxBackoff  time.Duration
	BaseBackoff time.Duration
}

// Client implementa domain.Tracker contra a API GraphQL do Linear.
type Client struct {
	endpoint string
	http     *http.Client
	auth     authorizer
	clock    domain.Clock
	log      *slog.Logger

	createAsUser string
	retries      int
	maxBackoff   time.Duration
	baseBackoff  time.Duration

	teamMu  sync.Mutex
	teamID  string
	teamKey string

	projectMu      sync.Mutex
	projectsByName map[string]string
	projectsLoaded bool

	labelMu      sync.Mutex
	labelsByName map[string]string
	labelsLoaded bool
}

var _ domain.Tracker = (*Client)(nil)

// New monta o Client. Falha cedo quando falta credencial ou time, porque descobrir
// isso na primeira Ocorrência da madrugada é o pior momento possível.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.TeamID) == "" && strings.TrimSpace(opts.TeamKey) == "" {
		return nil, errors.New("tracker: é preciso o time do Linear (TeamID ou TeamKey)")
	}

	httpc := opts.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	clock := opts.Clock
	if clock == nil {
		clock = domain.SystemClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	auth, err := newAuthorizer(opts, httpc, clock)
	if err != nil {
		return nil, err
	}

	createAs := opts.CreateAsUser
	if createAs == "" {
		createAs = DefaultCreateAsUser
	}
	// createAsUser só existe para aplicações OAuth em modo actor=app; mandar o campo
	// autenticado por chave pessoal faz a mutation inteira ser recusada.
	if !auth.actorApp() {
		createAs = ""
	}

	c := &Client{
		endpoint:       firstNonEmpty(opts.Endpoint, DefaultEndpoint),
		http:           httpc,
		auth:           auth,
		clock:          clock,
		log:            logger,
		createAsUser:   createAs,
		retries:        opts.Retries,
		maxBackoff:     opts.MaxBackoff,
		baseBackoff:    opts.BaseBackoff,
		teamID:         strings.TrimSpace(opts.TeamID),
		teamKey:        strings.TrimSpace(opts.TeamKey),
		labelsByName:   map[string]string{},
		projectsByName: map[string]string{},
	}
	if c.retries <= 0 {
		c.retries = 2
	}
	if c.maxBackoff <= 0 {
		c.maxBackoff = 30 * time.Second
	}
	if c.baseBackoff <= 0 {
		c.baseBackoff = time.Second
	}
	return c, nil
}

func newAuthorizer(opts Options, httpc *http.Client, clock domain.Clock) (authorizer, error) {
	id := strings.TrimSpace(opts.Secrets.LinearClientID)
	secret := strings.TrimSpace(opts.Secrets.LinearClientSecret)
	if id != "" && secret != "" {
		return &appToken{
			clientID:     id,
			clientSecret: secret,
			scope:        firstNonEmpty(opts.Scopes, DefaultScopes),
			endpoint:     firstNonEmpty(opts.TokenEndpoint, DefaultTokenEndpoint),
			http:         httpc,
			clock:        clock,
		}, nil
	}
	if key := strings.TrimSpace(opts.Secrets.LinearAPIKey); key != "" {
		return personalKey{key: key}, nil
	}
	return nil, fmt.Errorf("tracker: faltam credenciais do Linear (%s+%s ou %s)",
		config.EnvLinearClientID, config.EnvLinearClientSecret, config.EnvLinearAPIKey)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
