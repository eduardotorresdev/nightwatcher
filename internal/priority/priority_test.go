package priority_test

import (
	"fmt"
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/priority"
)

// This file is the specification of the Prioridade rule, and the PRD's one deliberate
// exception to the integration seam: the rule is pure, it is where calibration will
// happen over time, and it deserves a readable table of cases — facts in, priority out
// — instead of having its behaviour inferred from integration tests.
//
// Reading these tables should explain the rule without reading the code.

// The three shapes a previous Linear issue can be in. Openness comes from state.type
// and never from the state's name, because the team's states are customised and in
// Portuguese.
var (
	issueFechada = domain.IssueState{Exists: true, Open: false, StateType: "completed"}
	issueAberta  = domain.IssueState{Exists: true, Open: true, StateType: "started"}
	semIssue     = domain.IssueState{}
)

// Tabela de criação: o desfecho da Triagem e o level do Sentry, e nada mais. Volume e
// ambiente não existem neste instante — `issue.created` é disparado com gate is_new,
// então count e userCount valem ~1 por construção.
func TestAtCreationComesFromTheTriageVerdictAndTheSentryLevel(t *testing.T) {
	for _, tc := range []struct {
		verdict domain.Verdict
		level   domain.Level
		want    domain.Priority
	}{
		{domain.VerdictDefect, domain.LevelFatal, domain.PriorityHigh},
		{domain.VerdictDefect, domain.LevelError, domain.PriorityMedium},
		{domain.VerdictDefect, domain.LevelWarning, domain.PriorityLow},
		{domain.VerdictDefect, domain.LevelInfo, domain.PriorityLow},
		{domain.VerdictDefect, domain.LevelDebug, domain.PriorityLow},

		// Sugestão é Low seja qual for o level: um pedido de recurso não disputa
		// urgência com algo quebrado.
		{domain.VerdictSuggestion, domain.LevelFatal, domain.PriorityLow},
		{domain.VerdictSuggestion, domain.LevelError, domain.PriorityLow},
		{domain.VerdictSuggestion, domain.LevelInfo, domain.PriorityLow},
		{domain.VerdictSuggestion, "", domain.PriorityLow},

		// Dúvida e ruído encerram a Triagem e nunca chegam aqui. Se algum dia
		// chegarem, saem Low — a regra não tem saída que signifique "sem opinião",
		// porque 0 é indistinguível de bug na regra.
		{domain.VerdictQuestion, domain.LevelFatal, domain.PriorityLow},
		{domain.VerdictNoise, domain.LevelFatal, domain.PriorityLow},
		{"", domain.LevelFatal, domain.PriorityLow},

		// Level ausente ou desconhecido não inventa urgência.
		{domain.VerdictDefect, "", domain.PriorityLow},
		{domain.VerdictDefect, "critical", domain.PriorityLow},
	} {
		t.Run(fmt.Sprintf("%s/%s", tc.verdict, tc.level), func(t *testing.T) {
			got := priority.AtCreation(priority.Facts{
				Verdict:   tc.verdict,
				Level:     tc.level,
				Substatus: domain.SubstatusNew,
			})
			if got.Priority != tc.want {
				t.Errorf("%s com level %q nasce %s, quero %s", tc.verdict, tc.level, got.Priority, tc.want)
			}
		})
	}
}

// Volume NÃO entra na criação, e esta é a correção mais cara do PRD: `issue.created` é
// disparado com gate is_new, então count e userCount valem ~1 por construção.
// Priorizar por eles ali produziria sempre o mesmo valor — o defeito do antecessor com
// outra roupa. A faixa de volume é da segunda passada e só dela: por maior que seja o
// userCount entregue, a criação não se move.
func TestAtCreationNeverPrioritisesByVolume(t *testing.T) {
	for _, tc := range []struct {
		level domain.Level
		want  domain.Priority
	}{
		{domain.LevelWarning, domain.PriorityLow},
		{domain.LevelError, domain.PriorityMedium},
		{domain.LevelFatal, domain.PriorityHigh},
	} {
		for _, pessoas := range []int64{0, 1, 4, 5, 24, 25, 4000} {
			t.Run(fmt.Sprintf("%s/%d pessoas", tc.level, pessoas), func(t *testing.T) {
				got := priority.AtCreation(priority.Facts{
					Verdict:   domain.VerdictDefect,
					Level:     tc.level,
					Substatus: domain.SubstatusNew,
					UserCount: pessoas,
				})
				if got.Priority != tc.want {
					t.Errorf("defeito/%s com %d pessoas nasce %s, quero %s: a faixa de volume é da segunda passada", tc.level, pessoas, got.Priority, tc.want)
				}
			})
		}
	}
}

// Tabela de modificadores: um fato de cada vez, um nível de cada vez, teto em High.
func TestModifiersRaiseByOneLevelAndStopAtHigh(t *testing.T) {
	for _, tc := range []struct {
		name      string
		level     domain.Level
		substatus domain.Substatus
		previous  domain.IssueState
		want      domain.Priority
	}{
		// Sem modificador.
		{"new não move nada", domain.LevelError, domain.SubstatusNew, semIssue, domain.PriorityMedium},
		{"ongoing não move nada", domain.LevelError, domain.SubstatusOngoing, issueAberta, domain.PriorityMedium},

		// Escalating.
		{"escalating sobe um nível", domain.LevelError, domain.SubstatusEscalating, semIssue, domain.PriorityHigh},
		{"escalating sobre warning sobe para Medium", domain.LevelWarning, domain.SubstatusEscalating, semIssue, domain.PriorityMedium},
		{"escalating sobre fatal para em High, nunca vira Urgent", domain.LevelFatal, domain.SubstatusEscalating, semIssue, domain.PriorityHigh},

		// Regressão. Só sobe o que já tinha sido dado como resolvido.
		{"regressed com issue anterior fechada sobe um nível", domain.LevelError, domain.SubstatusRegressed, issueFechada, domain.PriorityHigh},
		{"regressed com issue anterior fechada sobre warning sobe para Medium", domain.LevelWarning, domain.SubstatusRegressed, issueFechada, domain.PriorityMedium},
		{"regressed com issue anterior aberta não sobe: ela só recebe comentário", domain.LevelError, domain.SubstatusRegressed, issueAberta, domain.PriorityMedium},
		{"regressed sem issue anterior conhecida não sobe: nada foi dado como resolvido", domain.LevelError, domain.SubstatusRegressed, semIssue, domain.PriorityMedium},
		{"regressed sobre fatal para em High", domain.LevelFatal, domain.SubstatusRegressed, issueFechada, domain.PriorityHigh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := priority.AtCreation(priority.Facts{
				Verdict:       domain.VerdictDefect,
				Level:         tc.level,
				Substatus:     tc.substatus,
				PreviousIssue: tc.previous,
			})
			if got.Priority != tc.want {
				t.Errorf("defeito/%s com substatus %q = %s, quero %s", tc.level, tc.substatus, got.Priority, tc.want)
			}
		})
	}
}

// O agente move no máximo um nível, para cima ou para baixo, e apenas com
// justificativa escrita. Sem justificativa o ajuste é recusado e a regra fica com o
// seu próprio valor.
func TestTheAgentMovesOneLevelAndOnlyWithAWrittenJustification(t *testing.T) {
	for _, tc := range []struct {
		name         string
		level        domain.Level
		adjustment   priority.Adjustment
		want         domain.Priority
		wantAdjusted bool
		wantRefused  bool
	}{
		{
			name:         "sobe um nível com justificativa",
			level:        domain.LevelWarning,
			adjustment:   priority.Adjustment{Move: priority.MoveUp, Justification: "perda silenciosa de dado do usuário"},
			want:         domain.PriorityMedium,
			wantAdjusted: true,
		},
		{
			name:         "desce um nível com justificativa",
			level:        domain.LevelError,
			adjustment:   priority.Adjustment{Move: priority.MoveDown, Justification: "erro de terceiro sem impacto no fluxo"},
			want:         domain.PriorityLow,
			wantAdjusted: true,
		},
		// Aceito não é o mesmo que mudou: saturar no teto ou no piso não desfaz o
		// aceite, e a justificativa continua indo para a issue.
		{
			name:         "teto em High: o agente nunca alcança Urgent",
			level:        domain.LevelFatal,
			adjustment:   priority.Adjustment{Move: priority.MoveUp, Justification: "derruba o checkout inteiro"},
			want:         domain.PriorityHigh,
			wantAdjusted: true,
		},
		{
			name:         "piso em Low: o agente nunca alcança No priority",
			level:        domain.LevelWarning,
			adjustment:   priority.Adjustment{Move: priority.MoveDown, Justification: "ruído conhecido de extensão de navegador"},
			want:         domain.PriorityLow,
			wantAdjusted: true,
		},
		{
			name:        "subir sem justificativa é recusado",
			level:       domain.LevelWarning,
			adjustment:  priority.Adjustment{Move: priority.MoveUp},
			want:        domain.PriorityLow,
			wantRefused: true,
		},
		{
			name:        "descer sem justificativa é recusado",
			level:       domain.LevelError,
			adjustment:  priority.Adjustment{Move: priority.MoveDown},
			want:        domain.PriorityMedium,
			wantRefused: true,
		},
		{
			name:        "justificativa em branco não é justificativa",
			level:       domain.LevelError,
			adjustment:  priority.Adjustment{Move: priority.MoveUp, Justification: "   \n\t "},
			want:        domain.PriorityMedium,
			wantRefused: true,
		},
		{
			name:       "sem movimento não há ajuste nem recusa",
			level:      domain.LevelError,
			adjustment: priority.Adjustment{},
			want:       domain.PriorityMedium,
		},
		{
			name:       "justificativa sem movimento não é ajuste",
			level:      domain.LevelError,
			adjustment: priority.Adjustment{Justification: "achei grave"},
			want:       domain.PriorityMedium,
		},
		{
			// Movimento que a regra não sabe nomear vira "manteve", não "recusou":
			// recusa é reservada para o movimento nomeado e não justificado.
			name:       "movimento desconhecido é tratado como manter",
			level:      domain.LevelError,
			adjustment: priority.Adjustment{Move: "aumentar", Justification: "derruba o pagamento"},
			want:       domain.PriorityMedium,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := priority.AtCreation(priority.Facts{
				Verdict:   domain.VerdictDefect,
				Level:     tc.level,
				Substatus: domain.SubstatusNew,
				Agent:     tc.adjustment,
			})
			if got.Priority != tc.want {
				t.Errorf("prioridade = %s, quero %s", got.Priority, tc.want)
			}
			if got.Adjusted() != tc.wantAdjusted {
				t.Errorf("ajuste aceito = %v, quero %v", got.Adjusted(), tc.wantAdjusted)
			}
			if got.Refused != tc.wantRefused {
				t.Errorf("recusado = %v, quero %v", got.Refused, tc.wantRefused)
			}
		})
	}
}

// A justificativa é levada adiante para ser escrita na issue: ler as justificativas é
// como a regra vai sendo calibrada com o tempo. O valor da regra sobrevive ao lado do
// ajuste, senão a issue não consegue dizer quem decidiu o quê.
func TestAnAcceptedAdjustmentCarriesItsJustificationAndKeepsTheRuleValueVisible(t *testing.T) {
	got := priority.AtCreation(priority.Facts{
		Verdict:   domain.VerdictDefect,
		Level:     domain.LevelWarning,
		Substatus: domain.SubstatusNew,
		Agent:     priority.Adjustment{Move: priority.MoveUp, Justification: "  apaga pedido pago sem aviso  "},
	})
	if !got.Adjusted() {
		t.Error("ajuste justificado deveria ter sido aceito")
	}
	if got.Justification != "apaga pedido pago sem aviso" {
		t.Errorf("justificativa = %q, quero o texto do agente sem espaço em volta", got.Justification)
	}
	if got.Rule != domain.PriorityLow {
		t.Errorf("valor da regra = %s, quero %s", got.Rule, domain.PriorityLow)
	}
	if got.Priority != domain.PriorityMedium {
		t.Errorf("prioridade = %s, quero %s", got.Priority, domain.PriorityMedium)
	}
}

// Um ajuste recusado não pode levar justificativa nenhuma adiante: se levasse, a issue
// exibiria a razão de um movimento que não aconteceu.
func TestARefusedAdjustmentCarriesNothingOntoTheIssue(t *testing.T) {
	got := priority.AtCreation(priority.Facts{
		Verdict:   domain.VerdictDefect,
		Level:     domain.LevelError,
		Substatus: domain.SubstatusNew,
		Agent:     priority.Adjustment{Move: priority.MoveUp},
	})
	if got.Adjusted() {
		t.Error("ajuste sem justificativa não deveria contar como aceito")
	}
	if got.Justification != "" {
		t.Errorf("justificativa = %q, quero vazia", got.Justification)
	}
	if got.Priority != got.Rule {
		t.Errorf("prioridade = %s, quero o valor da regra %s", got.Priority, got.Rule)
	}
}

// Tabela da segunda passada: a faixa é por PESSOAS ATINGIDAS. Ela é piso e nunca teto:
// abaixo de 5 pessoas não diz nada e o valor da criação permanece.
//
// Que a faixa não possa ser por eventos não é afirmável por teste de comportamento —
// é afirmado pelo tipo: Facts não carrega count nenhum, então não existe entrada
// "volume de eventos" que uma calibração futura possa ler por engano. count alto sem
// userCount alto é um usuário em laço apertado: barulhento, não grave.
func TestAtEscalationBandsByPeopleAffected(t *testing.T) {
	for _, tc := range []struct {
		level     domain.Level
		userCount int64
		want      domain.Priority
	}{
		// Criação em Low (defeito/warning): a faixa é o que move.
		{domain.LevelWarning, 0, domain.PriorityLow},
		{domain.LevelWarning, 1, domain.PriorityLow},
		{domain.LevelWarning, 4, domain.PriorityLow},
		{domain.LevelWarning, 5, domain.PriorityMedium},
		{domain.LevelWarning, 24, domain.PriorityMedium},
		{domain.LevelWarning, 25, domain.PriorityHigh},
		{domain.LevelWarning, 4000, domain.PriorityHigh},

		// Criação em Medium (defeito/error): a faixa de 5 não acrescenta nada, a de 25 sim.
		{domain.LevelError, 4, domain.PriorityMedium},
		{domain.LevelError, 5, domain.PriorityMedium},
		{domain.LevelError, 24, domain.PriorityMedium},
		{domain.LevelError, 25, domain.PriorityHigh},

		// Criação em High (defeito/fatal): não há para onde subir, o teto é High.
		{domain.LevelFatal, 0, domain.PriorityHigh},
		{domain.LevelFatal, 25, domain.PriorityHigh},
		{domain.LevelFatal, 100000, domain.PriorityHigh},
	} {
		t.Run(fmt.Sprintf("%s/%d pessoas", tc.level, tc.userCount), func(t *testing.T) {
			got := priority.AtEscalation(priority.Facts{
				Verdict:   domain.VerdictDefect,
				Level:     tc.level,
				Substatus: domain.SubstatusEscalating,
				UserCount: tc.userCount,
			}, domain.PriorityNone)
			if got.Priority != tc.want {
				t.Errorf("defeito/%s com %d pessoas atingidas = %s, quero %s", tc.level, tc.userCount, got.Priority, tc.want)
			}
		})
	}
}

// A regressão continua contando na segunda passada. A faixa de volume fala pelo
// `escalating` — é o mesmo fato, agora com número — mas não fala por "isto já tinha
// sido dado como resolvido", que é outro fato e não é absorvido pelo primeiro.
func TestAtEscalationStillRaisesARegressionOfSomethingAlreadyClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous domain.IssueState
		want     domain.Priority
	}{
		{"issue anterior fechada sobe um nível", issueFechada, domain.PriorityHigh},
		{"issue anterior aberta não sobe: ela só recebe comentário", issueAberta, domain.PriorityMedium},
		{"sem issue anterior não sobe: nada foi dado como resolvido", semIssue, domain.PriorityMedium},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := priority.AtEscalation(priority.Facts{
				Verdict:       domain.VerdictDefect,
				Level:         domain.LevelError,
				Substatus:     domain.SubstatusRegressed,
				PreviousIssue: tc.previous,
				UserCount:     4, // abaixo da faixa: quem move aqui é a regressão
			}, domain.PriorityNone)
			if got.Priority != tc.want {
				t.Errorf("regressão na segunda passada = %s, quero %s", got.Priority, tc.want)
			}
		})
	}
}

// A Prioridade nunca cai por conta própria: quem baixou na mão baixou por um motivo que
// o daemon não conhece. Só um valor MAIS ALTO é escrito.
func TestAtEscalationOnlyEverRaisesWhatIsAlreadyRecorded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		level     domain.Level
		userCount int64
		recorded  domain.Priority
		want      domain.Priority
	}{
		{"resultado mais alto que o registrado é escrito", domain.LevelWarning, 25, domain.PriorityLow, domain.PriorityHigh},
		{"resultado igual ao registrado não muda nada", domain.LevelError, 25, domain.PriorityHigh, domain.PriorityHigh},
		{"resultado mais baixo que o registrado é descartado", domain.LevelWarning, 0, domain.PriorityMedium, domain.PriorityMedium},
		{"nada registrado ainda: o resultado vale", domain.LevelWarning, 5, domain.PriorityNone, domain.PriorityMedium},
		{"Urgent posto por uma pessoa não é desfeito pela regra", domain.LevelFatal, 5000, domain.PriorityUrgent, domain.PriorityUrgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := priority.AtEscalation(priority.Facts{
				Verdict:   domain.VerdictDefect,
				Level:     tc.level,
				Substatus: domain.SubstatusEscalating,
				UserCount: tc.userCount,
			}, tc.recorded)
			if got.Priority != tc.want {
				t.Errorf("registrado %s, recalculado para %s, quero %s", tc.recorded, got.Priority, tc.want)
			}
		})
	}
}

// Nem o ajuste justificado do agente derruba o que já está registrado: o agente é o
// daemon, e a proteção existe contra o daemon, não contra a pessoa.
func TestAtEscalationDoesNotLetTheAgentLowerWhatIsRecorded(t *testing.T) {
	got := priority.AtEscalation(priority.Facts{
		Verdict:   domain.VerdictDefect,
		Level:     domain.LevelWarning,
		Substatus: domain.SubstatusEscalating,
		UserCount: 5,
		Agent:     priority.Adjustment{Move: priority.MoveDown, Justification: "só afeta uma tela secundária"},
	}, domain.PriorityMedium)
	if got.Priority != domain.PriorityMedium {
		t.Errorf("prioridade = %s, quero %s: prioridade não cai sozinha", got.Priority, domain.PriorityMedium)
	}
}

// 0 — No priority nunca é produzido: prioridade ausente é indistinguível de bug na
// regra e o board perde sua ordenação. 1 — Urgent nunca é produzido automaticamente:
// dispara e-mail urgente e SLA de 24h, e um falso positivo custa a confiança no sistema
// inteiro. Foi disso que o antecessor morreu. Urgent é ato humano.
func TestTheRuleNeverProducesNoPriorityNorUrgent(t *testing.T) {
	verdicts := []domain.Verdict{domain.VerdictDefect, domain.VerdictSuggestion, domain.VerdictQuestion, domain.VerdictNoise, ""}
	levels := []domain.Level{domain.LevelFatal, domain.LevelError, domain.LevelWarning, domain.LevelInfo, domain.LevelDebug, ""}
	substatuses := []domain.Substatus{domain.SubstatusNew, domain.SubstatusRegressed, domain.SubstatusEscalating, domain.SubstatusOngoing, ""}
	previous := []domain.IssueState{semIssue, issueAberta, issueFechada}
	counts := []int64{0, 1, 4, 5, 24, 25, 1 << 40}
	moves := []priority.Adjustment{
		{},
		{Move: priority.MoveUp, Justification: "porque sim"},
		{Move: priority.MoveDown, Justification: "porque não"},
		{Move: priority.MoveUp},
	}

	for _, v := range verdicts {
		for _, l := range levels {
			for _, s := range substatuses {
				for _, prev := range previous {
					for _, c := range counts {
						for _, a := range moves {
							f := priority.Facts{Verdict: v, Level: l, Substatus: s, PreviousIssue: prev, UserCount: c, Agent: a}
							check := func(what string, got domain.Priority) {
								t.Helper()
								switch got {
								case domain.PriorityHigh, domain.PriorityMedium, domain.PriorityLow:
								default:
									t.Fatalf("%s produziu %s para %+v", what, got, f)
								}
							}
							check("AtCreation", priority.AtCreation(f).Priority)
							// Na escalada só o que já estava registrado pode ser
							// Urgent, e aí ele é preservado de propósito.
							check("AtEscalation", priority.AtEscalation(f, domain.PriorityNone).Priority)
						}
					}
				}
			}
		}
	}
}
