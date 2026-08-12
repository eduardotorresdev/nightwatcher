package tracker_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// A suíte de integração afirma sobre o espião, então o espião precisa ser confiável
// antes dela: o que ele diz que aconteceu é o que o teste vai chamar de verdade.
func TestTheSpyRecordsWhatWasCreatedInOrder(t *testing.T) {
	var spy tracker.Spy // o valor zero já funciona
	ctx := context.Background()

	first, err := spy.CreateIssue(ctx, domain.IssueRequest{
		LinearProjectID: "projeto-web",
		Title:           "primeira",
		Body:            "corpo da primeira",
		Priority:        domain.PriorityHigh,
		Labels:          []string{"web"},
		AttachmentURL:   "https://sentry.io/x/1/",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := spy.CreateIssue(ctx, domain.IssueRequest{
		LinearProjectID: "projeto-api",
		Title:           "segunda",
		Priority:        domain.PriorityLow,
		RelatedTo:       first.ID,
	}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	created := spy.Created()
	if len(created) != 2 {
		t.Fatalf("issues criadas = %d, quero 2", len(created))
	}
	if created[0].Title != "primeira" || created[1].Title != "segunda" {
		t.Errorf("a ordem se perdeu: %q, %q", created[0].Title, created[1].Title)
	}
	if created[0].ProjectID != "projeto-web" || created[1].ProjectID != "projeto-api" {
		t.Error("o Project de cada issue precisa ser o que foi pedido")
	}
	if created[0].Priority != domain.PriorityHigh || created[1].Priority != domain.PriorityLow {
		t.Error("a Prioridade precisa chegar intacta ao espião")
	}
	if len(created[0].Labels) != 1 || created[0].Labels[0] != "web" {
		t.Errorf("rótulos = %v", created[0].Labels)
	}
	if created[0].Body != "corpo da primeira" {
		t.Errorf("corpo = %q", created[0].Body)
	}
	if created[0].AttachmentURL != "https://sentry.io/x/1/" {
		t.Errorf("anexo = %q", created[0].AttachmentURL)
	}
	if created[1].RelatedTo != first.ID {
		t.Errorf("relação = %q, quero %q", created[1].RelatedTo, first.ID)
	}
	if first.ID == "" || first.Identifier == "" || first.URL == "" {
		t.Errorf("a IssueRef devolvida precisa ser utilizável: %+v", first)
	}

	// Mexer no que Created() devolveu não pode mexer no que o espião guardou.
	created[0].Labels[0] = "mexido"
	if again := spy.Created(); again[0].Labels[0] != "web" {
		t.Error("Created() devolveu o rótulo por referência")
	}
}

func TestTheSpyRecordsCommentsByIssue(t *testing.T) {
	spy := tracker.NewSpy()
	ctx := context.Background()

	if err := spy.Comment(ctx, "ACME-1", "primeiro"); err != nil {
		t.Fatal(err)
	}
	if err := spy.Comment(ctx, "ACME-2", "de outra issue"); err != nil {
		t.Fatal(err)
	}
	if err := spy.Comment(ctx, "ACME-1", "segundo"); err != nil {
		t.Fatal(err)
	}

	got := spy.Comments("ACME-1")
	if len(got) != 2 || got[0] != "primeiro" || got[1] != "segundo" {
		t.Errorf("comentários de ACME-1 = %v", got)
	}
	if len(spy.Comments("ACME-3")) != 0 {
		t.Error("issue sem comentário precisa devolver vazio")
	}
	if spy.CommentCount() != 3 {
		t.Errorf("total de comentários = %d, quero 3", spy.CommentCount())
	}
	if all := spy.AllComments(); len(all) != 3 || all[1].IssueID != "ACME-2" {
		t.Errorf("AllComments = %v", all)
	}
}

func TestTheSpyAnswersIssueStateForTheRecurrencePath(t *testing.T) {
	spy := tracker.NewSpy()
	ctx := context.Background()

	// Nunca vista: é isto que faz o caminho de reincidência criar issue nova.
	got, err := spy.IssueState(ctx, "desconhecida")
	if err != nil {
		t.Fatal(err)
	}
	if got.Exists {
		t.Error("issue desconhecida não existe")
	}

	ref, err := spy.CreateIssue(ctx, domain.IssueRequest{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = spy.IssueState(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exists || !got.Open {
		t.Errorf("issue recém-criada tem que estar aberta: %+v", got)
	}
	if same, _ := spy.IssueState(ctx, ref.Identifier); !same.Exists {
		t.Error("o espião precisa responder também pelo identificador")
	}

	spy.Close(ref.ID)
	got, _ = spy.IssueState(ctx, ref.ID)
	if !got.Exists || got.Open {
		t.Errorf("depois de Close a issue existe e está fechada: %+v", got)
	}
	if got.StateType != "completed" {
		t.Errorf("StateType = %q", got.StateType)
	}
	if byKey, _ := spy.IssueState(ctx, ref.Identifier); byKey.Open {
		t.Error("Close precisa valer pelas duas chaves")
	}
}

func TestTheSpyServesCannedIssueStates(t *testing.T) {
	spy := tracker.NewSpy()
	want := domain.IssueState{Exists: true, Open: false, StateType: "canceled", Identifier: "ACME-9"}
	spy.SetIssueState("ACME-9", want)

	got, err := spy.IssueState(context.Background(), "ACME-9")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("IssueState = %+v, quero %+v", got, want)
	}
}

func TestTheSpyCanFailOnPurpose(t *testing.T) {
	spy := tracker.NewSpy()
	ctx := context.Background()
	boom := errors.New("o Destino está fora do ar")

	spy.FailCreate(boom)
	if _, err := spy.CreateIssue(ctx, domain.IssueRequest{Title: "t"}); !errors.Is(err, boom) {
		t.Errorf("CreateIssue devolveu %v", err)
	}
	spy.FailComment(boom)
	if err := spy.Comment(ctx, "i", "c"); !errors.Is(err, boom) {
		t.Errorf("Comment devolveu %v", err)
	}
	spy.FailIssueState(boom)
	if _, err := spy.IssueState(ctx, "i"); !errors.Is(err, boom) {
		t.Errorf("IssueState devolveu %v", err)
	}

	spy.FailCreate(nil)
	if _, err := spy.CreateIssue(ctx, domain.IssueRequest{Title: "t"}); err != nil {
		t.Errorf("voltar ao normal: %v", err)
	}
}

// O worker roda Análises em paralelo: um espião com corrida seria um teste instável
// em vez de um bug do sistema.
func TestTheSpyIsSafeUnderConcurrentUse(t *testing.T) {
	spy := tracker.NewSpy()
	ctx := context.Background()

	const n = 25
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ref, err := spy.CreateIssue(ctx, domain.IssueRequest{Title: fmt.Sprintf("t%d", i)})
			if err != nil {
				t.Errorf("CreateIssue: %v", err)
				return
			}
			if err := spy.Comment(ctx, ref.ID, "comentário"); err != nil {
				t.Errorf("Comment: %v", err)
			}
			if _, err := spy.IssueState(ctx, ref.ID); err != nil {
				t.Errorf("IssueState: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if spy.CreatedCount() != n {
		t.Errorf("issues criadas = %d, quero %d", spy.CreatedCount(), n)
	}
	if spy.CommentCount() != n {
		t.Errorf("comentários = %d, quero %d", spy.CommentCount(), n)
	}
	seen := map[string]bool{}
	for _, c := range spy.Created() {
		if seen[c.Ref.ID] {
			t.Fatalf("id repetido: %s", c.Ref.ID)
		}
		seen[c.Ref.ID] = true
	}
}

func TestTheSpyResets(t *testing.T) {
	spy := tracker.NewSpy()
	ctx := context.Background()
	if _, err := spy.CreateIssue(ctx, domain.IssueRequest{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := spy.Comment(ctx, "i", "c"); err != nil {
		t.Fatal(err)
	}
	spy.Reset()
	if spy.CreatedCount() != 0 || spy.CommentCount() != 0 {
		t.Error("Reset precisa esvaziar o espião")
	}
	if _, ok := spy.LastCreated(); ok {
		t.Error("depois do Reset não há última issue")
	}
}
