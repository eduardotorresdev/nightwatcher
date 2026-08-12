package config_test

import (
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
)

// The operating numbers are typed by hand on the command line and stored in a file an
// operator can edit. Every value that reaches Tunables through Apply is therefore
// hostile input, and the ones below are the shapes that turn into a defect the daemon
// would act on in silence rather than into an error where somebody is looking.
func TestApplyRefusesValuesThatWouldBreakTheDaemonSilently(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"a lease of zero lets every worker steal every item", "lease_ttl", "0s"},
		{"a negative lease is the same defect written differently", "lease_ttl", "-5m"},
		{"a Triagem ceiling of zero times out every session", "triage_timeout", "0s"},
		{"a Diagnóstico ceiling of zero times out every session", "deep_timeout", "0"},
		{"a negative Diagnóstico ceiling", "deep_timeout", "-1m"},
		{"a duration without a unit is not a duration", "deep_timeout", "20"},
		{"concurrency below one claims nothing", "concurrency", "0"},
		{"a trailing typo must not be read as the number before it", "concurrency", "5x"},
		{"a duration typed into a count key", "max_attempts", "2m"},
		{"an unknown key", "não_existe", "3"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tun := config.Defaults()
			if err := tun.Apply(tc.key, tc.value); err == nil {
				t.Fatalf("Apply(%q, %q) foi aceito; devia ser recusado", tc.key, tc.value)
			}
			if !sameNumbers(tun, config.Defaults()) {
				// A rejected value that still moved the field is worse than one that was
				// accepted: nothing downstream knows the number changed.
				t.Errorf("Apply(%q, %q) recusou mas mexeu nos números", tc.key, tc.value)
			}
		})
	}
}

func TestApplyAcceptsTheOperatingNumbers(t *testing.T) {
	cases := []struct {
		key   string
		value string
		want  func(config.Tunables) bool
	}{
		{"concurrency", "4", func(t config.Tunables) bool { return t.Concurrency == 4 }},
		{"triage_timeout", "90s", func(t config.Tunables) bool { return t.TriageTimeout == 90*time.Second }},
		{"deep_timeout", "45m", func(t config.Tunables) bool { return t.DeepTimeout == 45*time.Minute }},
		{"max_attempts", "5", func(t config.Tunables) bool { return t.MaxAttempts == 5 }},
		{"per_project_hourly", "50", func(t config.Tunables) bool { return t.PerProjectHourly == 50 }},
		{"max_queue", "500", func(t config.Tunables) bool { return t.MaxQueue == 500 }},
		{"lease_ttl", "30m", func(t config.Tunables) bool { return t.LeaseTTL == 30*time.Minute }},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			tun := config.Defaults()
			if err := tun.Apply(tc.key, tc.value); err != nil {
				t.Fatalf("Apply(%q, %q): %v", tc.key, tc.value, err)
			}
			if !tc.want(tun) {
				t.Errorf("Apply(%q, %q) não gravou o valor", tc.key, tc.value)
			}
		})
	}
}

// Every key `config set` offers has to be a key Apply knows, or the command line
// advertises a knob that does not exist.
func TestEverySettableKeyIsAppliable(t *testing.T) {
	for _, key := range config.Settable {
		tun := config.Defaults()
		if err := tun.Apply(key, "1m"); err != nil {
			// Count keys reject "1m"; what must not happen is "chave desconhecida".
			if err2 := tun.Apply(key, "7"); err2 != nil {
				t.Errorf("chave %q não é aplicável nem como duração nem como número: %v / %v", key, err, err2)
			}
		}
	}
}

// The backoff table is indexed by attempt number and read past its end by the retry
// path; a wait of zero would turn "come back later" into a hot loop.
func TestBackoffForNeverReturnsZero(t *testing.T) {
	tun := config.Defaults()
	for attempt := -1; attempt <= len(tun.Backoff)+3; attempt++ {
		if d := tun.BackoffFor(attempt); d <= 0 {
			t.Errorf("BackoffFor(%d) = %s; espera precisa ser positiva", attempt, d)
		}
	}
}

// sameNumbers compares the scalar operating numbers. Tunables carries a slice, so it
// is not comparable with ==.
func sameNumbers(a, b config.Tunables) bool {
	return a.Concurrency == b.Concurrency &&
		a.TriageTimeout == b.TriageTimeout &&
		a.DeepTimeout == b.DeepTimeout &&
		a.MaxAttempts == b.MaxAttempts &&
		a.PerProjectHourly == b.PerProjectHourly &&
		a.MaxQueue == b.MaxQueue &&
		a.LeaseTTL == b.LeaseTTL
}
