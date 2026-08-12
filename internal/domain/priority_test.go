package domain_test

import (
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// The Linear scale is inverted and 0 sits outside the order. These tests exist
// because `p - 1` reads like "less urgent" and does the opposite — the single
// mistake most likely to ship a wrong priority silently.
func TestRaiseMovesTowardsUrgentAndStopsAtTheAutomaticCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		from domain.Priority
		by   int
		want domain.Priority
	}{
		{"Low sobe para Medium", domain.PriorityLow, 1, domain.PriorityMedium},
		{"Medium sobe para High", domain.PriorityMedium, 1, domain.PriorityHigh},
		{"High não passa de High: Urgent é ato humano", domain.PriorityHigh, 1, domain.PriorityHigh},
		{"Low sobe dois e para em High", domain.PriorityLow, 2, domain.PriorityHigh},
		{"Low sobe muitos e ainda para em High", domain.PriorityLow, 9, domain.PriorityHigh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.from.Raise(tc.by); got != tc.want {
				t.Errorf("%s.Raise(%d) = %s, quero %s", tc.from, tc.by, got, tc.want)
			}
		})
	}
}

func TestLowerMovesAwayFromUrgentAndStopsAtLow(t *testing.T) {
	for _, tc := range []struct {
		name string
		from domain.Priority
		by   int
		want domain.Priority
	}{
		{"High desce para Medium", domain.PriorityHigh, 1, domain.PriorityMedium},
		{"Medium desce para Low", domain.PriorityMedium, 1, domain.PriorityLow},
		{"Low não vira No priority", domain.PriorityLow, 1, domain.PriorityLow},
		{"Low continua Low por mais que desça", domain.PriorityLow, 5, domain.PriorityLow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.from.Lower(tc.by); got != tc.want {
				t.Errorf("%s.Lower(%d) = %s, quero %s", tc.from, tc.by, got, tc.want)
			}
		})
	}
}

func TestMoreUrgentThanReadsThroughTheInvertedScale(t *testing.T) {
	if !domain.PriorityHigh.MoreUrgentThan(domain.PriorityLow) {
		t.Error("High deveria ser mais urgente que Low")
	}
	if domain.PriorityLow.MoreUrgentThan(domain.PriorityHigh) {
		t.Error("Low não deveria ser mais urgente que High")
	}
	if domain.PriorityNone.MoreUrgentThan(domain.PriorityLow) {
		t.Error("No priority é o menos urgente que existe, apesar de ser o menor número")
	}
	if !domain.PriorityLow.MoreUrgentThan(domain.PriorityNone) {
		t.Error("qualquer prioridade real supera No priority")
	}
}

func TestParseVerdictAcceptsTheUnaccentedAliasButKeepsTheCanonicalValue(t *testing.T) {
	for _, in := range []string{"sugestão", "sugestao", "SUGESTAO"} {
		got, err := domain.ParseVerdict(in)
		if err != nil {
			t.Fatalf("ParseVerdict(%q) devolveu erro: %v", in, err)
		}
		if got != domain.VerdictSuggestion {
			t.Errorf("ParseVerdict(%q) = %q, quero %q", in, got, domain.VerdictSuggestion)
		}
	}
	if _, err := domain.ParseVerdict("qualquer coisa"); err == nil {
		t.Error("desfecho desconhecido deveria falhar")
	}
}

func TestOnlyDefectAndSuggestionCarryOn(t *testing.T) {
	for v, want := range map[domain.Verdict]bool{
		domain.VerdictDefect:     true,
		domain.VerdictSuggestion: true,
		domain.VerdictQuestion:   false,
		domain.VerdictNoise:      false,
	} {
		if got := v.CarriesOn(); got != want {
			t.Errorf("%q.CarriesOn() = %v, quero %v", v, got, want)
		}
	}
}
