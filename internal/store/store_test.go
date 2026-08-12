package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

// t0 is the fixed instant every test measures from. Nothing in the store reads the
// wall clock, so a test can put a lease three weeks in the past without waiting.
var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Setup(context.Background(), filepath.Join(t.TempDir(), "nightwatcher.db"), t0)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// fixtureProject registers a Projeto with one Fonte, which is what the foreign keys
// on analyses and queue require before anything else can be written.
func fixtureProject(t *testing.T, s *store.Store, name, slug string) domain.Project {
	t.Helper()
	p, err := s.UpsertProject(context.Background(), domain.Project{
		Name:            name,
		Repo:            "acme/" + name,
		Branch:          "main",
		LinearProjectID: "lp-" + name,
		Sources:         []domain.Source{{SentrySlug: slug, Label: "api"}},
	}, t0)
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	return p
}

func fixtureOccurrence(issueID string, sub domain.Substatus) domain.Occurrence {
	return domain.Occurrence{
		InstallationUUID:  "inst-1",
		SentryProjectID:   "4507",
		IssueID:           issueID,
		Substatus:         sub,
		Action:            domain.ActionCreated,
		Category:          domain.CategoryError,
		Level:             domain.LevelError,
		Title:             "TypeError: cannot read properties of undefined",
		Culprit:           "app/checkout.ts",
		Permalink:         "https://sentry.io/organizations/acme/issues/" + issueID + "/",
		ShortID:           "ACME-" + issueID,
		Count:             1,
		UserCount:         1,
		FirstSeen:         t0.Add(-time.Hour),
		LastSeen:          t0,
		SentryProjectSlug: "acme",
		RawBody:           []byte(`{"action":"created"}`),
		ReceivedAt:        t0,
	}
}

func fixtureAnalysis(t *testing.T, s *store.Store, p domain.Project, issueID string) domain.Analysis {
	t.Helper()
	a, err := s.CreateAnalysis(context.Background(), domain.Analysis{
		ProjectID:  p.ID,
		SourceID:   p.Sources[0].ID,
		State:      domain.StateQueued,
		Occurrence: fixtureOccurrence(issueID, domain.SubstatusNew),
	}, t0)
	if err != nil {
		t.Fatalf("CreateAnalysis: %v", err)
	}
	return a
}

func TestMigrateRunTwiceLeavesTheDataAlone(t *testing.T) {
	// Every process that opens the database migrates on startup, and `dokku run`
	// opens it while the daemon is already running.
	path := filepath.Join(t.TempDir(), "nightwatcher.db")
	ctx := context.Background()

	first, err := store.Setup(ctx, path, t0)
	if err != nil {
		t.Fatalf("primeiro Setup: %v", err)
	}
	fixtureProject(t, first, "acme", "acme-api")
	first.Close()

	second, err := store.Setup(ctx, path, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("segundo Setup: %v", err)
	}
	defer second.Close()

	projects, err := second.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "acme" {
		t.Fatalf("depois de migrar de novo: %+v", projects)
	}
}

func TestSeveralProcessesOpeningTheSameNewDatabaseAtOnceAllSucceed(t *testing.T) {
	// This is a Dokku deploy: the new daemon boots while a `dokku run` container is
	// already opening the same file, and on a fresh volume neither has migrated yet.
	// Two things break here if they are not handled — switching the file to WAL wants
	// an exclusive lock and is the one statement SQLite will not route through the
	// busy handler, and a migration that decides "already applied?" outside its own
	// transaction lets both processes decide "no" and the loser die on `table
	// projects already exists`.
	path := filepath.Join(t.TempDir(), "nightwatcher.db")

	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := store.Setup(context.Background(), path, t0)
			if err != nil {
				errs[i] = err
				return
			}
			defer s.Close()
			// Whoever got here has a usable schema, not just an open handle.
			_, errs[i] = s.ListProjects(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Setup concorrente %d: %v", i, err)
		}
	}
}

func TestResolveSourceFindsTheProjetoBehindASentrySlug(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	fixtureProject(t, s, "acme", "acme-api")

	p, src, err := s.ResolveSource(ctx, "acme-api")
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	if p.Name != "acme" || p.Repo != "acme/acme" {
		t.Errorf("resolveu para o projeto %+v", p)
	}
	if src.Label != "api" {
		t.Errorf("rótulo da fonte = %q, quero api", src.Label)
	}

	// An unknown slug is the "fonte desconhecida" recusa, and has to be
	// distinguishable from a broken query.
	if _, _, err := s.ResolveSource(ctx, "nao-cadastrado"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("slug desconhecido devolveu %v, quero ErrNotFound", err)
	}
}

func TestTwoFontesCanFeedTheSameProjeto(t *testing.T) {
	// The predecessor's config already had two Sentry projects pointing at one
	// repository, which is what killed the one-Fonte-per-Projeto premise.
	ctx := context.Background()
	s := newStore(t)

	p := fixtureProject(t, s, "acme", "acme-api")
	if _, err := s.UpsertSource(ctx, domain.Source{ProjectID: p.ID, SentrySlug: "acme-web", Label: "web"}); err != nil {
		t.Fatalf("UpsertSource: %v", err)
	}

	web, _, err := s.ResolveSource(ctx, "acme-web")
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	api, _, err := s.ResolveSource(ctx, "acme-api")
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	if web.ID != api.ID {
		t.Errorf("fontes caíram em projetos diferentes: %d e %d", web.ID, api.ID)
	}
}

func TestUpsertProjectCorrectsAProjetoInsteadOfDuplicatingIt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	fixtureProject(t, s, "acme", "acme-api")
	if _, err := s.UpsertProject(ctx, domain.Project{
		Name:            "acme",
		Repo:            "acme/acme",
		Branch:          "develop",
		LinearProjectID: "lp-acme",
	}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("UpsertProject de novo: %v", err)
	}

	p, err := s.ProjectByName(ctx, "acme")
	if err != nil {
		t.Fatalf("ProjectByName: %v", err)
	}
	if p.Branch != "develop" {
		t.Errorf("branch = %q, quero develop", p.Branch)
	}
	// The Fonte was not repeated on the second call and must still be there: a
	// forgotten flag cannot unhook a Fonte that is still delivering.
	if len(p.Sources) != 1 || p.Sources[0].SentrySlug != "acme-api" {
		t.Errorf("fontes depois do upsert: %+v", p.Sources)
	}
}

func TestStoredSettingsOverlayOntoTheDefaults(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.SetTunable(ctx, "concurrency", "5"); err != nil {
		t.Fatalf("SetTunable concurrency: %v", err)
	}
	if err := s.SetTunable(ctx, "deep_timeout", "45m"); err != nil {
		t.Fatalf("SetTunable deep_timeout: %v", err)
	}
	// The table also carries settings that are not operating numbers; the loader has
	// to walk past them instead of failing.
	if err := s.SetSetting(ctx, "linear_team", "ACME"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	got, err := s.Tunables(ctx)
	if err != nil {
		t.Fatalf("Tunables: %v", err)
	}
	if got.Concurrency != 5 {
		t.Errorf("Concurrency = %d, quero 5", got.Concurrency)
	}
	if got.DeepTimeout != 45*time.Minute {
		t.Errorf("DeepTimeout = %v, quero 45m", got.DeepTimeout)
	}
	if want := config.Defaults(); got.MaxQueue != want.MaxQueue || got.LeaseTTL != want.LeaseTTL {
		t.Errorf("o que não foi ajustado saiu do padrão: MaxQueue=%d LeaseTTL=%v", got.MaxQueue, got.LeaseTTL)
	}
}

func TestSetTunableRefusesWhatWouldOnlyFailAtTheNextStart(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.SetTunable(ctx, "deep_timeout", "vinte minutos"); err == nil {
		t.Error("valor sem unidade foi aceito")
	}
	if err := s.SetTunable(ctx, "concurrencia", "2"); err == nil {
		t.Error("chave inexistente foi aceita")
	}

	// Nothing was stored, so the daemon still starts on the defaults.
	got, err := s.Tunables(ctx)
	if err != nil {
		t.Fatalf("Tunables: %v", err)
	}
	if got.DeepTimeout != config.Defaults().DeepTimeout {
		t.Errorf("DeepTimeout = %v depois de uma recusa", got.DeepTimeout)
	}
}

func TestAHandEditedSettingThatDoesNotParseIsAnError(t *testing.T) {
	// Written past SetTunable, the way `sqlite3` on the volume would. Falling back to
	// the default silently would hide the mistake behind numbers nobody chose.
	ctx := context.Background()
	s := newStore(t)

	if err := s.SetSetting(ctx, "max_attempts", "muitas"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := s.Tunables(ctx); err == nil {
		t.Error("Tunables aceitou max_attempts inválido")
	}
}
