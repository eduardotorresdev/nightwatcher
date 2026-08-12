package tracker

import (
	"context"
	"fmt"
	"sync"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// Spy é o Tracker em memória sobre o qual a suíte de integração afirma. O PRD define
// o Tracker como "o mundo externo" para efeito de teste, então este tipo existe para
// responder exatamente às perguntas que os cenários fazem: qual issue nasceu, em qual
// Project, com qual Prioridade, com qual conteúdo, e qual comentário caiu em qual
// issue.
//
// O valor zero já funciona. É seguro para uso concorrente porque o worker roda
// Análises em paralelo e um espião com corrida seria um teste instável, não um bug
// do sistema.
type Spy struct {
	mu       sync.Mutex
	created  []CreatedIssue
	comments []PostedComment
	states   map[string]domain.IssueState
	seq      int

	createErr  error
	commentErr error
	stateErr   error
}

// CreatedIssue é uma issue que o espião viu nascer, com tudo o que a decidiu.
type CreatedIssue struct {
	ProjectID       string
	Title           string
	Body            string
	Priority        domain.Priority
	Labels          []string
	RelatedTo       string
	AttachmentURL   string
	AttachmentTitle string

	// Ref é o que o espião devolveu ao chamador.
	Ref domain.IssueRef
}

// PostedComment é um comentário que o espião viu cair numa issue.
type PostedComment struct {
	IssueID string
	Body    string
}

var _ domain.Tracker = (*Spy)(nil)

// NewSpy existe por simetria com o resto do pacote; o valor zero é igualmente válido.
func NewSpy() *Spy { return &Spy{} }

func (s *Spy) CreateIssue(_ context.Context, req domain.IssueRequest) (domain.IssueRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.createErr != nil {
		return domain.IssueRef{}, s.createErr
	}

	s.seq++
	ref := domain.IssueRef{
		ID:         fmt.Sprintf("issue-%d", s.seq),
		Identifier: fmt.Sprintf("NW-%d", s.seq),
	}
	ref.URL = "https://linear.test/issue/" + ref.Identifier

	s.created = append(s.created, CreatedIssue{
		ProjectID:       req.LinearProjectID,
		Title:           req.Title,
		Body:            req.Body,
		Priority:        req.Priority,
		Labels:          append([]string(nil), req.Labels...),
		RelatedTo:       req.RelatedTo,
		AttachmentURL:   req.AttachmentURL,
		AttachmentTitle: req.AttachmentTitle,
		Ref:             ref,
	})

	// A issue recém-criada passa a existir e a estar aberta, para que o cenário de
	// reincidência possa consultá-la logo em seguida sem preparar nada.
	open := domain.IssueState{
		Exists:     true,
		Open:       true,
		StateType:  "backlog",
		Identifier: ref.Identifier,
		URL:        ref.URL,
	}
	s.setStateLocked(ref.ID, open)
	s.setStateLocked(ref.Identifier, open)

	return ref, nil
}

func (s *Spy) Comment(_ context.Context, issueID, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commentErr != nil {
		return s.commentErr
	}
	s.comments = append(s.comments, PostedComment{IssueID: issueID, Body: body})
	return nil
}

func (s *Spy) IssueState(_ context.Context, issueID string) (domain.IssueState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateErr != nil {
		return domain.IssueState{}, s.stateErr
	}
	if st, ok := s.states[issueID]; ok {
		return st, nil
	}
	// Desconhecida é "nunca criei issue para isto", que é o que faz o caminho de
	// reincidência criar uma nova.
	return domain.IssueState{}, nil
}

// Created devolve as issues criadas, na ordem em que nasceram.
func (s *Spy) Created() []CreatedIssue {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CreatedIssue, len(s.created))
	for i, c := range s.created {
		c.Labels = append([]string(nil), c.Labels...)
		out[i] = c
	}
	return out
}

// CreatedCount é o número de issues criadas — a asserção mais comum dos cenários de
// deduplicação.
func (s *Spy) CreatedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.created)
}

// LastCreated devolve a última issue criada.
func (s *Spy) LastCreated() (CreatedIssue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.created) == 0 {
		return CreatedIssue{}, false
	}
	c := s.created[len(s.created)-1]
	c.Labels = append([]string(nil), c.Labels...)
	return c, true
}

// Comments devolve os corpos dos comentários feitos numa issue, em ordem.
func (s *Spy) Comments(issueID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.comments {
		if c.IssueID == issueID {
			out = append(out, c.Body)
		}
	}
	return out
}

// AllComments devolve todos os comentários, na ordem, com a issue de cada um.
func (s *Spy) AllComments() []PostedComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PostedComment(nil), s.comments...)
}

// CommentCount é o total de comentários feitos.
func (s *Spy) CommentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.comments)
}

// SetIssueState fixa a resposta de IssueState para uma issue.
func (s *Spy) SetIssueState(issueID string, st domain.IssueState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStateLocked(issueID, st)
}

// Close marca a issue como fechada por conclusão — o gatilho do cenário de
// reincidência depois de fechada. Fecha também pelo identificador, para o teste não
// precisar saber qual das duas chaves o sistema guardou.
func (s *Spy) Close(issueID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[issueID]
	if !ok {
		st = domain.IssueState{Exists: true, Identifier: issueID}
	}
	st.Exists = true
	st.Open = false
	st.StateType = "completed"
	s.setStateLocked(issueID, st)
	if st.Identifier != "" && st.Identifier != issueID {
		s.setStateLocked(st.Identifier, st)
	}
	for _, c := range s.created {
		if c.Ref.ID == issueID || c.Ref.Identifier == issueID {
			s.setStateLocked(c.Ref.ID, st)
			s.setStateLocked(c.Ref.Identifier, st)
		}
	}
}

// FailCreate, FailComment e FailIssueState fazem o espião falhar, para o cenário em
// que o próprio Destino está fora do ar. Passar nil volta ao normal.
func (s *Spy) FailCreate(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createErr = err
}

func (s *Spy) FailComment(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commentErr = err
}

func (s *Spy) FailIssueState(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateErr = err
}

// Reset esvazia o espião, para um teste de tabela reutilizar a instância.
func (s *Spy) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = nil
	s.comments = nil
	s.states = nil
	s.seq = 0
	s.createErr, s.commentErr, s.stateErr = nil, nil, nil
}

func (s *Spy) setStateLocked(issueID string, st domain.IssueState) {
	if issueID == "" {
		return
	}
	if s.states == nil {
		s.states = map[string]domain.IssueState{}
	}
	s.states[issueID] = st
}
