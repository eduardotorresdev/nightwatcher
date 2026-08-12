package tracker_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// Este é o teste mais importante do adaptador. Os estados do time são customizados e
// em português, então um adaptador que decidisse por nome classificaria errado em
// silêncio — e "aberta" contra "fechada" é o que separa comentar de abrir issue nova.
// O Destino já em forma de UUID atravessa sem consulta nenhuma.
const projectUUID = "3171aa9a-29ef-4ddf-a4ba-48d96f7a8184"

func TestIssueStateIsDecidedByStateTypeAndNeverByTheStateName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stateName string
		stateType string
		wantOpen  bool
	}{
		{"nome em português, trabalho começado", "Em desenvolvimento", "started", true},
		{"nome em português, no backlog", "Banco de tarefas", "backlog", true},
		{"na triagem do time", "Triagem", "triage", true},
		{"ainda não começada", "A fazer", "unstarted", true},
		{"o mesmo nome que aparece aberto, mas concluída", "Em produção", "completed", false},
		{"cancelada", "Não vamos fazer", "canceled", false},
		{"duplicada", "Duplicada", "duplicate", false},
		{"nome que soa terminal, mas o tipo diz que não é", "Concluído?", "started", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLinear(t)
			f.onData("IssueStateByID", fmt.Sprintf(
				`{"issues":{"nodes":[{"id":"11111111-2222-3333-4444-555555555555","identifier":"ACME-1",
				  "url":"https://linear.app/x/issue/ACME-1","archivedAt":null,
				  "state":{"id":"s1","name":%q,"type":%q}}]}}`, tc.stateName, tc.stateType))

			got, err := newClient(t, f, newFakeClock()).
				IssueState(context.Background(), "11111111-2222-3333-4444-555555555555")
			if err != nil {
				t.Fatalf("IssueState: %v", err)
			}
			if !got.Exists {
				t.Fatal("a issue existe e o adaptador disse que não")
			}
			if got.Open != tc.wantOpen {
				t.Errorf("estado %q/%q: Open = %v, quero %v", tc.stateName, tc.stateType, got.Open, tc.wantOpen)
			}
			if got.StateType != tc.stateType {
				t.Errorf("StateType = %q, quero %q", got.StateType, tc.stateType)
			}
		})
	}
}

// Uma issue arquivada some das consultas por padrão. Se sumir, o daemon lê "nunca
// criei issue para esta Ocorrência" e abre uma duplicata.
func TestIssueStateLooksInsideTheArchive(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("IssueStateByID", `{"issues":{"nodes":[{"id":"11111111-2222-3333-4444-555555555555",
	  "identifier":"ACME-9","url":"https://linear.app/x/issue/ACME-9","archivedAt":"2026-01-02T03:04:05.000Z",
	  "state":{"id":"s1","name":"Banco de tarefas","type":"backlog"}}]}}`)

	got, err := newClient(t, f, newFakeClock()).
		IssueState(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if !got.Exists {
		t.Fatal("a issue arquivada precisa ser encontrada, ou nasce uma duplicata")
	}
	if q := f.only("IssueStateByID").Query; !strings.Contains(q, "includeArchived: true") {
		t.Errorf("a consulta precisa pedir arquivadas; veio:\n%s", q)
	}
}

func TestIssueStateReportsAnIssueThatDoesNotExist(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("IssueStateByID", `{"issues":{"nodes":[]}}`)
	f.on("IssueStateByKey", func(w http.ResponseWriter, _ gqlRequest) {
		writeBadRequestWithCode(w, "Entity not found: Issue", "ENTITY_NOT_FOUND")
	})

	got, err := newClient(t, f, newFakeClock()).
		IssueState(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("issue inexistente é resposta, não erro: %v", err)
	}
	if got.Exists {
		t.Error("Exists deveria ser falso")
	}
}

// O identificador humano não casa no filtro por id, então precisa do campo raiz.
func TestIssueStateAcceptsAHumanIdentifier(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("IssueStateByKey", `{"issue":{"id":"uuid-1","identifier":"ACME-346",
	  "url":"https://linear.app/x/issue/ACME-346","archivedAt":null,
	  "state":{"id":"s1","name":"Em produção","type":"completed"}}}`)

	got, err := newClient(t, f, newFakeClock()).IssueState(context.Background(), "ACME-346")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if !got.Exists || got.Open {
		t.Errorf("quero existente e fechada, veio %+v", got)
	}
	if got.Identifier != "ACME-346" {
		t.Errorf("Identifier = %q", got.Identifier)
	}
}

func TestCreateIssueSendsProjectPriorityBodyAndLabel(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("TeamLabels", `{"team":{"id":"team-uuid","labels":{"nodes":[
	  {"id":"label-api","name":"api"},{"id":"label-web","name":"web"}]}}}`)
	f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{
	  "id":"issue-uuid","identifier":"ACME-500","url":"https://linear.app/x/issue/ACME-500"}}}`)
	f.onData("CreateAttachment", `{"attachmentCreate":{"success":true,"attachment":{"id":"a1","url":"u"}}}`)

	ref, err := newClient(t, f, newFakeClock()).CreateIssue(context.Background(), domain.IssueRequest{
		LinearProjectID: projectUUID,
		Title:           "TypeError: cannot read property 'id' of undefined",
		Body:            "## Diagnóstico\n\ncausa provável",
		Priority:        domain.PriorityHigh,
		Labels:          []string{"API"},
		AttachmentURL:   "https://sentry.io/organizations/acme/issues/42/",
		AttachmentTitle: "Ocorrência no Sentry",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if ref.ID != "issue-uuid" || ref.Identifier != "ACME-500" || ref.URL == "" {
		t.Errorf("IssueRef = %+v", ref)
	}

	in := f.only("CreateIssue").input(t)
	if in["projectId"] != projectUUID {
		t.Errorf("projectId = %v", in["projectId"])
	}
	if in["teamId"] != "team-uuid" {
		t.Errorf("teamId = %v", in["teamId"])
	}
	if in["description"] != "## Diagnóstico\n\ncausa provável" {
		t.Errorf("description = %v", in["description"])
	}
	// "API" e "api" são o mesmo rótulo: criar os dois parte o filtro do board em dois.
	if got := labelIDs(t, in); len(got) != 1 || got[0] != "label-api" {
		t.Errorf("labelIds = %v, quero [label-api]", got)
	}
	if len(f.got("CreateLabel")) != 0 {
		t.Error("o rótulo já existia; não deveria ter sido criado de novo")
	}
	if url := f.only("CreateAttachment").input(t)["url"]; url != "https://sentry.io/organizations/acme/issues/42/" {
		t.Errorf("o anexo foi criado com url %v", url)
	}
}

// A escala do Linear é invertida e o 0 fica fora da ordem: o valor tem que chegar lá
// exatamente como saiu do cálculo de Prioridade.
func TestCreateIssuePassesThePriorityThroughUntouched(t *testing.T) {
	for _, p := range []domain.Priority{
		domain.PriorityNone, domain.PriorityUrgent, domain.PriorityHigh,
		domain.PriorityMedium, domain.PriorityLow,
	} {
		t.Run(p.String(), func(t *testing.T) {
			f := newFakeLinear(t)
			f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)

			if _, err := newClient(t, f, newFakeClock()).CreateIssue(context.Background(), domain.IssueRequest{
				LinearProjectID: projectUUID, Title: "t", Priority: p,
			}); err != nil {
				t.Fatalf("CreateIssue: %v", err)
			}

			got, ok := f.only("CreateIssue").input(t)["priority"].(float64)
			if !ok {
				t.Fatalf("priority não veio como número")
			}
			if int(got) != int(p) {
				t.Errorf("priority enviada = %d, quero %d (%s)", int(got), int(p), p)
			}
		})
	}
}

// O Projeto guarda o Destino pelo nome que se lê no board, e o Linear só aceita
// UUID. Mandar o nome cru recusa a mutation inteira — foi o que segurou toda Análise
// fora do board em 2026-08-12.
func TestTheDestinationNameIsTranslatedIntoTheProjectUUID(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("TeamProjects", `{"team":{"id":"team-uuid","projects":{"nodes":[
	  {"id":"`+projectUUID+`","name":"acmE"},{"id":"outro-uuid","name":"executor"}]}}}`)
	f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)

	c := newClient(t, f, newFakeClock())
	for i := 0; i < 2; i++ {
		// A caixa do nome configurado não precisa bater com a do board.
		if _, err := c.CreateIssue(context.Background(), domain.IssueRequest{
			LinearProjectID: "acme", Title: "t",
		}); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
	}

	for _, req := range f.got("CreateIssue") {
		if got := req.input(t)["projectId"]; got != projectUUID {
			t.Errorf("projectId = %v, quero %s", got, projectUUID)
		}
	}
	if n := len(f.got("TeamProjects")); n != 1 {
		t.Errorf("os Projects foram lidos %d vezes; uma basta", n)
	}
}

// Destino é enfeite do board; a issue é o registro da Ocorrência. Um nome que não
// existe mais no Linear não pode custar a Ocorrência.
func TestAnUnknownDestinationStillLetsTheIssueBeBorn(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("TeamProjects", `{"team":{"id":"team-uuid","projects":{"nodes":[]}}}`)
	f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)

	ref, err := newClient(t, f, newFakeClock()).CreateIssue(context.Background(), domain.IssueRequest{
		LinearProjectID: "acme", Title: "t",
	})
	if err != nil {
		t.Fatalf("a issue tinha que nascer mesmo fora do Project: %v", err)
	}
	if ref.ID != "i" {
		t.Errorf("IssueRef = %+v", ref)
	}
	if _, ok := f.only("CreateIssue").input(t)["projectId"]; ok {
		t.Error("sem Project resolvido, projectId não deveria ser enviado")
	}
}

func TestCreateIssueCreatesTheSourceLabelOnDemand(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("TeamLabels", `{"team":{"id":"team-uuid","labels":{"nodes":[]}}}`)
	f.onData("CreateLabel", `{"issueLabelCreate":{"success":true,"issueLabel":{"id":"label-novo","name":"api"}}}`)
	f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)

	if _, err := newClient(t, f, newFakeClock()).CreateIssue(context.Background(), domain.IssueRequest{
		LinearProjectID: projectUUID, Title: "t", Labels: []string{"api"},
	}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if name := f.only("CreateLabel").Vars["name"]; name != "api" {
		t.Errorf("rótulo criado com nome %v", name)
	}
	if got := labelIDs(t, f.only("CreateIssue").input(t)); len(got) != 1 || got[0] != "label-novo" {
		t.Errorf("labelIds = %v, quero [label-novo]", got)
	}
}

// Rótulo é enfeite; a issue é o registro da Ocorrência. Perder o registro por causa do
// enfeite é exatamente o defeito que matou o antecessor.
func TestCreateIssueStillCreatesTheIssueWhenTheLabelFails(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("TeamLabels", `{"team":{"id":"team-uuid","labels":{"nodes":[]}}}`)
	f.on("CreateLabel", func(w http.ResponseWriter, _ gqlRequest) {
		writeBadRequest(w, "label name is reserved")
	})
	f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)

	ref, err := newClient(t, f, newFakeClock()).CreateIssue(context.Background(), domain.IssueRequest{
		LinearProjectID: projectUUID, Title: "t", Labels: []string{"api"},
	})
	if err != nil {
		t.Fatalf("a issue tinha que nascer mesmo sem o rótulo: %v", err)
	}
	if ref.ID != "i" {
		t.Errorf("IssueRef = %+v", ref)
	}
	if _, ok := f.only("CreateIssue").input(t)["labelIds"]; ok {
		t.Error("sem rótulo resolvido, labelIds não deveria ser enviado")
	}
}

// Reincidência depois de fechada: a issue nova aponta para a anterior.
func TestCreateIssueRelatesToThePreviousIssue(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"nova","identifier":"ACME-2","url":"u"}}}`)
	f.onData("RelateIssues", `{"issueRelationCreate":{"success":true,"issueRelation":{"id":"r1","type":"related"}}}`)

	if _, err := newClient(t, f, newFakeClock()).CreateIssue(context.Background(), domain.IssueRequest{
		LinearProjectID: projectUUID, Title: "t", RelatedTo: "antiga",
	}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	rel := f.only("RelateIssues")
	if rel.Vars["issueId"] != "nova" || rel.Vars["relatedIssueId"] != "antiga" {
		t.Errorf("relação criada com %v", rel.Vars)
	}
	// `related` e não `similar`: `similar` existe no enum mas não aparece em nenhuma
	// página de produto, e relação que a UI não mostra é relação que ninguém lê.
	if !strings.Contains(rel.Query, "type: related") {
		t.Errorf("a relação tem que ser `related`; a mutation foi:\n%s", rel.Query)
	}
}

func TestCommentLandsOnTheGivenIssue(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("CreateComment", `{"commentCreate":{"success":true,"comment":{"id":"c1","url":"u"}}}`)

	if err := newClient(t, f, newFakeClock()).
		Comment(context.Background(), "issue-uuid", "voltou a acontecer"); err != nil {
		t.Fatalf("Comment: %v", err)
	}

	in := f.only("CreateComment").input(t)
	if in["issueId"] != "issue-uuid" || in["body"] != "voltou a acontecer" {
		t.Errorf("comentário enviado com %v", in)
	}
}

func TestCommentSurfacesTheApiError(t *testing.T) {
	f := newFakeLinear(t)
	f.on("CreateComment", func(w http.ResponseWriter, _ gqlRequest) {
		writeBadRequest(w, "issue not writable")
	})

	err := newClient(t, f, newFakeClock()).Comment(context.Background(), "issue-uuid", "corpo")
	if err == nil {
		t.Fatal("quero erro")
	}
	var apiErr *tracker.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("quero *tracker.APIError, veio %T", err)
	}
	if apiErr.Retryable() {
		t.Error("400 comum não é retentável")
	}
}

func labelIDs(t *testing.T, input map[string]any) []string {
	t.Helper()
	raw, ok := input["labelIds"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, fmt.Sprint(v))
	}
	return out
}

// `createAsUser` só existe para aplicação OAuth em modo actor=app. Mandá-lo
// autenticado por chave pessoal faz o Linear recusar a mutation inteira: a issue não
// nasce e a Ocorrência se perde exatamente onde ela deveria aparecer.
func TestCreateAsUserOnlyTravelsInAppMode(t *testing.T) {
	t.Run("chave pessoal omite o campo", func(t *testing.T) {
		f := newFakeLinear(t)
		f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)
		f.onData("CreateComment", `{"commentCreate":{"success":true,"comment":{"id":"c1","url":"u"}}}`)

		c := newClient(t, f, newFakeClock())
		if _, err := c.CreateIssue(context.Background(), domain.IssueRequest{Title: "t"}); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if err := c.Comment(context.Background(), "i", "corpo"); err != nil {
			t.Fatalf("Comment: %v", err)
		}
		if _, ok := f.only("CreateIssue").input(t)["createAsUser"]; ok {
			t.Error("com chave pessoal o Linear recusa createAsUser; ele não pode ser enviado")
		}
		if _, ok := f.only("CreateComment").input(t)["createAsUser"]; ok {
			t.Error("o mesmo vale para o comentário")
		}
	})

	t.Run("conta de serviço assina como nightwatcher", func(t *testing.T) {
		f := newFakeLinear(t)
		f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)

		c := newAppClient(t, f, newFakeOAuth(t, 30*24*time.Hour), newFakeClock())
		if _, err := c.CreateIssue(context.Background(), domain.IssueRequest{Title: "t"}); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if got := f.only("CreateIssue").input(t)["createAsUser"]; got != tracker.DefaultCreateAsUser {
			t.Errorf("createAsUser = %v, quero %q", got, tracker.DefaultCreateAsUser)
		}
	})
}

// A repetição interna da chamada (cota, rede) não pode fazer nascer uma segunda
// issue. O id vem do cliente e é o MESMO nas duas tentativas — se ele fosse gerado
// por tentativa, ou não fosse enviado, a queda de rede depois da mutation
// duplicaria a issue.
func TestTheSameIdempotencyKeyTravelsOnEveryRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   string
		data string
		call func(*tracker.Client) error
	}{
		{
			name: "issueCreate",
			op:   "CreateIssue",
			data: `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`,
			call: func(c *tracker.Client) error {
				_, err := c.CreateIssue(context.Background(), domain.IssueRequest{Title: "t"})
				return err
			},
		},
		{
			name: "commentCreate",
			op:   "CreateComment",
			data: `{"commentCreate":{"success":true,"comment":{"id":"c1","url":"u"}}}`,
			call: func(c *tracker.Client) error {
				return c.Comment(context.Background(), "issue-uuid", "corpo")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			f := newFakeLinear(t)
			var n int
			f.on(tc.op, func(w http.ResponseWriter, _ gqlRequest) {
				n++
				if n == 1 {
					// A primeira tentativa morre depois de o Linear já ter podido
					// gravar; é esse o caso que duplica.
					writeRateLimited(w, clock.Now().Add(time.Second))
					return
				}
				writeData(w, tc.data)
			})

			if err := tc.call(newClient(t, f, clock)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			got := f.got(tc.op)
			if len(got) != 2 {
				t.Fatalf("tentativas = %d, quero 2", len(got))
			}
			first, ok := got[0].input(t)["id"].(string)
			if !ok || !isUUIDv4(first) {
				t.Fatalf("a chamada precisa levar um UUID v4 do cliente, veio %v", got[0].input(t)["id"])
			}
			if second := got[1].input(t)["id"]; second != first {
				t.Errorf("a repetição levou id %v, quero o mesmo %q — id novo é registro duplicado", second, first)
			}
		})
	}
}

// Anexo e relação são enfeites da issue, não a issue. Devolver erro faria o worker
// retentar e criar uma segunda issue idêntica — o oposto do objetivo.
func TestTheIssueIsBornEvenWhenTheAttachmentAndTheRelationFail(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)
	f.on("CreateAttachment", func(w http.ResponseWriter, _ gqlRequest) { writeBadRequest(w, "url inválida") })
	f.on("RelateIssues", func(w http.ResponseWriter, _ gqlRequest) { writeBadRequest(w, "issue inexistente") })

	ref, err := newClient(t, f, newFakeClock()).CreateIssue(context.Background(), domain.IssueRequest{
		Title:         "t",
		AttachmentURL: "https://sentry.io/organizations/acme/issues/42/",
		RelatedTo:     "antiga",
	})
	if err != nil {
		t.Fatalf("a issue tinha que nascer mesmo sem anexo e sem relação: %v", err)
	}
	if ref.ID != "i" {
		t.Errorf("IssueRef = %+v", ref)
	}
	if len(f.got("CreateIssue")) != 1 {
		t.Errorf("a issue foi criada %d vezes", len(f.got("CreateIssue")))
	}
}

// O Destino pode ser configurado pela chave humana do time ("ACME"). Resolver custa
// uma consulta, e ela não pode se repetir a cada Ocorrência da madrugada.
func TestTheTeamKeyIsResolvedOnceAndReused(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("TeamByKey", `{"teams":{"nodes":[{"id":"team-uuid","key":"ACME","name":"Acme"}]}}`)
	f.onData("CreateIssue", `{"issueCreate":{"success":true,"issue":{"id":"i","identifier":"ACME-1","url":"u"}}}`)

	c := newClientWith(t, f, newFakeClock(), func(o *tracker.Options) {
		o.TeamID, o.TeamKey = "", "ACME"
	})
	for i := 0; i < 2; i++ {
		if _, err := c.CreateIssue(context.Background(), domain.IssueRequest{Title: "t"}); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
	}

	if got := f.only("TeamByKey").Vars["key"]; got != "ACME" {
		t.Errorf("a consulta foi pela chave %v", got)
	}
	for i, r := range f.got("CreateIssue") {
		if got := r.input(t)["teamId"]; got != "team-uuid" {
			t.Errorf("issue %d nasceu com teamId %v, quero o UUID resolvido", i, got)
		}
	}
}

func TestAnUnknownTeamKeyIsRefusedWithItsName(t *testing.T) {
	f := newFakeLinear(t)
	f.onData("TeamByKey", `{"teams":{"nodes":[]}}`)

	c := newClientWith(t, f, newFakeClock(), func(o *tracker.Options) {
		o.TeamID, o.TeamKey = "", "XXX"
	})
	_, err := c.CreateIssue(context.Background(), domain.IssueRequest{Title: "t"})
	if err == nil {
		t.Fatal("quero erro")
	}
	if !strings.Contains(err.Error(), "XXX") {
		t.Errorf("o erro precisa nomear o time procurado: %v", err)
	}
	if len(f.got("CreateIssue")) != 0 {
		t.Error("sem time não pode sair issue")
	}
}

// `config validate` existe para falar de coisa alcançada. Validar sem tocar na API
// aprovaria uma chave revogada, que é o formato de validação que não protege nada.
func TestValidateProvesTheCredentialReachesTheTeam(t *testing.T) {
	t.Run("credencial boa", func(t *testing.T) {
		f := newFakeLinear(t)
		f.onData("TeamByID", `{"team":{"id":"team-uuid","key":"ACME","name":"Acme"}}`)

		if err := newClient(t, f, newFakeClock()).Validate(context.Background()); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if got := f.only("TeamByID").Vars["teamId"]; got != "team-uuid" {
			t.Errorf("consultou o time %v", got)
		}
	})

	t.Run("credencial revogada", func(t *testing.T) {
		f := newFakeLinear(t)
		f.on("TeamByID", func(w http.ResponseWriter, _ gqlRequest) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"message":"Authentication required"}]}`)
		})

		if err := newClient(t, f, newFakeClock()).Validate(context.Background()); err == nil {
			t.Fatal("uma chave que o Linear recusa não pode passar na validação")
		}
	})
}

// Confundir uma falha de consulta com "essa issue nunca existiu" faz o caminho de
// reincidência abrir uma issue duplicada — em silêncio, que é o pior modo.
func TestOnlyARealAbsenceCountsAsIssueNotFound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		message    string
		code       string
		wantErr    bool
		wantExists bool
	}{
		{
			name:    "entidade ausente é resposta",
			message: "Entity not found: Issue - Could not find referenced Issue.",
			code:    "ENTITY_NOT_FOUND",
		},
		{
			name:    "campo escrito errado na query é falha nossa, não issue ausente",
			message: `Argument "filtro" not found on field Query.issue`,
			code:    "GRAPHQL_VALIDATION_FAILED",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLinear(t)
			f.on("IssueStateByKey", func(w http.ResponseWriter, _ gqlRequest) {
				writeBadRequestWithCode(w, tc.message, tc.code)
			})

			got, err := newClient(t, f, newFakeClock()).IssueState(context.Background(), "ACME-1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("quero erro; veio %+v — o worker abriria uma duplicata", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("issue inexistente é resposta, não erro: %v", err)
			}
			if got.Exists != tc.wantExists {
				t.Errorf("Exists = %v, quero %v", got.Exists, tc.wantExists)
			}
		})
	}
}
