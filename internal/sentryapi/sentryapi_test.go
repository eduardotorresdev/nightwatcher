package sentryapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/sentryapi"
)

func TestEnvironmentsReadsTheRecordedTagsResponse(t *testing.T) {
	body, err := os.ReadFile("../../testdata/sentry/tags-environment-7318899062.json")
	if err != nil {
		t.Fatalf("lendo fixture: %v", err)
	}

	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	c := sentryapi.New("tok-de-teste")
	c.BaseURL = srv.URL

	envs, err := c.Environments(context.Background(), "7318899062")
	if err != nil {
		t.Fatalf("Environments: %v", err)
	}
	if len(envs) != 1 || envs[0] != "development" {
		t.Errorf("environments = %v, quero [development]", envs)
	}
	if gotAuth != "Bearer tok-de-teste" {
		t.Errorf("Authorization = %q; o Sentry usa Bearer (ao contrário do Linear)", gotAuth)
	}
	if gotPath != "/issues/7318899062/tags/environment/" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestNotKnowingTheEnvironmentIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"The requested resource does not exist"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := sentryapi.New("tok")
	c.BaseURL = srv.URL

	envs, err := c.Environments(context.Background(), "999")
	if err != nil {
		t.Fatalf("404 na tag deveria ser silêncio, não erro: %v", err)
	}
	if len(envs) != 0 {
		t.Errorf("environments = %v, quero vazio", envs)
	}
}

func TestAServerErrorIsReportedRatherThanSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := sentryapi.New("tok")
	c.BaseURL = srv.URL

	if _, err := c.Environments(context.Background(), "1"); err == nil {
		t.Error("500 do Sentry deveria virar erro")
	}
}
