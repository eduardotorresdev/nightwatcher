package prompts

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

func TestParseTriageReadsTheFourOutcomes(t *testing.T) {
	cases := []struct {
		reply string
		want  domain.Verdict
	}{
		{`{"desfecho":"defeito","justificativa":"exceção capturada em produção"}`, domain.VerdictDefect},
		{`{"desfecho":"sugestão","justificativa":"pedido de exportar em CSV"}`, domain.VerdictSuggestion},
		{`{"desfecho":"dúvida","justificativa":"pergunta como emitir a nota"}`, domain.VerdictQuestion},
		{`{"desfecho":"ruído","justificativa":"mensagem com uma letra só"}`, domain.VerdictNoise},
		// The unaccented alias is accepted: rejecting it would fail an analysis over
		// a keyboard, and the canonical accented form is what gets stored.
		{`{"desfecho":"sugestao","justificativa":"pedido de recurso"}`, domain.VerdictSuggestion},
	}
	for _, tc := range cases {
		got, err := ParseTriage(tc.reply)
		if err != nil {
			t.Fatalf("%s: %v", tc.reply, err)
		}
		if got.Verdict != tc.want {
			t.Errorf("%s: desfecho %q, esperava %q", tc.reply, got.Verdict, tc.want)
		}
		if got.Rationale == "" {
			t.Errorf("%s: justificativa perdida", tc.reply)
		}
	}
}

// The model wraps the object in a markdown fence often enough that absorbing it is
// worth more than being right about the instruction.
func TestParseAcceptsRepliesWrappedInMarkdownFences(t *testing.T) {
	reply := "Aqui está a triagem:\n\n```json\n{\"desfecho\": \"defeito\", \"justificativa\": \"stack trace real\"}\n```\n"
	got, err := ParseTriage(reply)
	if err != nil {
		t.Fatalf("resposta em fence deveria ser aceita: %v", err)
	}
	if got.Verdict != domain.VerdictDefect {
		t.Errorf("desfecho %q", got.Verdict)
	}
}

func TestMalformedRepliesAreMalformedAndNotRetried(t *testing.T) {
	cases := map[string]string{
		"vazia":                 "",
		"prosa":                 "Acho que isso é um defeito no cálculo do total.",
		"JSON truncado":         `{"desfecho":"defeito","justificativa":`,
		"desfecho inventado":    `{"desfecho":"talvez","justificativa":"não sei"}`,
		"sem justificativa":     `{"desfecho":"ruído","justificativa":"   "}`,
		"objeto sem desfecho":   `{"justificativa":"faltou o desfecho"}`,
		"lista em vez de obj":   `[{"desfecho":"defeito"}]`,
		"lixo depois do JSON":   `{"desfecho":"defeito","justificativa":"ok"} e mais um pouco}`,
		"JSON em prosa solta":   `o resultado é {"desfecho":"defeito","justificativa":"ok"} pronto`,
		"fence sem JSON dentro": "```\nnão é JSON\n```",
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseTriage(reply)
			if err == nil {
				t.Fatalf("esperava falha")
			}
			if kind := domain.ClassifyAgentError(err); kind != domain.AgentErrMalformed {
				t.Errorf("classificou como %q, esperava %q", kind, domain.AgentErrMalformed)
			}
			if domain.ClassifyAgentError(err).Retryable() {
				t.Errorf("resposta malformada não pode ser retentada")
			}
		})
	}
}

func TestParseDiagnosisKeepsTheSuspectFiles(t *testing.T) {
	reply := `{
	  "causa_provavel": "calcularTotal assume que o pedido tem itens",
	  "arquivos_suspeitos": [
	    {"caminho": "src/services/pedido.ts", "linha": 128, "porque": "acessa itens[0] sem checar"},
	    {"caminho": "src/http/pedido-controller.ts", "porque": "chama calcularTotal com o corpo cru"}
	  ],
	  "evidencias": ["const total = pedido.itens[0].valor", "  "],
	  "confianca": "alta",
	  "ajuste_prioridade": {"direcao": "manter", "justificativa": ""}
	}`
	got, err := ParseDiagnosis(reply)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ProbableCause == "" {
		t.Error("causa provável perdida")
	}
	if len(got.SuspectFiles) != 2 {
		t.Fatalf("arquivos suspeitos: %d", len(got.SuspectFiles))
	}
	if got.SuspectFiles[0].Path != "src/services/pedido.ts" || got.SuspectFiles[0].Line != 128 {
		t.Errorf("primeiro arquivo: %+v", got.SuspectFiles[0])
	}
	if got.SuspectFiles[1].Line != 0 {
		t.Errorf("linha ausente deveria virar 0, veio %d", got.SuspectFiles[1].Line)
	}
	if len(got.Evidence) != 1 {
		t.Errorf("evidência vazia deveria ser descartada: %#v", got.Evidence)
	}
	if got.Confidence != ConfidenceHigh {
		t.Errorf("confiança %q", got.Confidence)
	}
}

// A diagnosis that names no file is the predecessor's failure with better
// formatting: it describes the symptom and leaves the investigation with the human.
func TestDiagnosisWithoutARealPathIsRejected(t *testing.T) {
	cases := map[string]string{
		"sem arquivos":      `{"causa_provavel":"o pedido falha","arquivos_suspeitos":[],"confianca":"baixa"}`,
		"caminho em branco": `{"causa_provavel":"o pedido falha","arquivos_suspeitos":[{"caminho":"   "}],"confianca":"baixa"}`,
		"sem causa":         `{"causa_provavel":"","arquivos_suspeitos":[{"caminho":"a.ts"}]}`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDiagnosis(reply); err == nil {
				t.Fatal("esperava falha")
			} else if domain.ClassifyAgentError(err) != domain.AgentErrMalformed {
				t.Errorf("classificou como %q", domain.ClassifyAgentError(err))
			}
		})
	}
}

func TestCheckPathsFindsWhatTheAgentInvented(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "pedido.ts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := CheckPaths(root, []SuspectFile{
		{Path: "src/pedido.ts"},
		{Path: "src/inventado.ts"},
		{Path: "/etc/passwd"},
		{Path: "../../fora.ts"},
	})
	want := []string{"src/inventado.ts", "/etc/passwd", "../../fora.ts"}
	if len(missing) != len(want) {
		t.Fatalf("faltantes %#v, esperava %#v", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Errorf("faltante[%d] = %q, esperava %q", i, missing[i], want[i])
		}
	}
}

// The PRD admits the agent's judgement "apenas com justificativa escrita". An
// unjustified move is therefore not an adjustment — but it is not a reason to throw
// away the investigation either.
func TestAdjustmentWithoutJustificationIsDropped(t *testing.T) {
	reply := `{"causa_provavel":"c","arquivos_suspeitos":[{"caminho":"a.ts"}],
	           "ajuste_prioridade":{"direcao":"subir","justificativa":"  "}}`
	got, err := ParseDiagnosis(reply)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Adjustment.Direction != AdjustKeep {
		t.Errorf("direção %q, esperava %q", got.Adjustment.Direction, AdjustKeep)
	}
	if p := got.Adjustment.Apply(domain.PriorityMedium); p != domain.PriorityMedium {
		t.Errorf("prioridade mudou para %s sem justificativa", p)
	}
}

func TestAdjustmentMovesOneLevelAndSaturates(t *testing.T) {
	cases := []struct {
		reply string
		base  domain.Priority
		want  domain.Priority
	}{
		{`{"direcao":"subir","justificativa":"perde dado em silêncio"}`, domain.PriorityMedium, domain.PriorityHigh},
		{`{"direcao":"descer","justificativa":"só afeta ambiente de testes"}`, domain.PriorityMedium, domain.PriorityLow},
		// The ceiling is the rule's, not the agent's: `1 — Urgent` is a human act.
		{`{"direcao":"subir","justificativa":"muito grave"}`, domain.PriorityHigh, domain.PriorityHigh},
		{`{"direcao":"descer","justificativa":"irrelevante"}`, domain.PriorityLow, domain.PriorityLow},
		{`{"direcao":"manter","justificativa":""}`, domain.PriorityMedium, domain.PriorityMedium},
	}
	for _, tc := range cases {
		reply := `{"causa_provavel":"c","arquivos_suspeitos":[{"caminho":"a.ts"}],"ajuste_prioridade":` + tc.reply + `}`
		got, err := ParseDiagnosis(reply)
		if err != nil {
			t.Fatalf("%s: %v", tc.reply, err)
		}
		if p := got.Adjustment.Apply(tc.base); p != tc.want {
			t.Errorf("%s: %s virou %s, esperava %s", tc.reply, tc.base, p, tc.want)
		}
	}
}

func TestUnknownAdjustmentDirectionIsMalformed(t *testing.T) {
	reply := `{"causa_provavel":"c","arquivos_suspeitos":[{"caminho":"a.ts"}],
	           "ajuste_prioridade":{"direcao":"urgente","justificativa":"agora"}}`
	if _, err := ParseDiagnosis(reply); err == nil {
		t.Fatal("esperava falha")
	} else if domain.ClassifyAgentError(err) != domain.AgentErrMalformed {
		t.Errorf("classificou como %q", domain.ClassifyAgentError(err))
	}
}

func TestParseStudyRequiresExactlyOneRecommendation(t *testing.T) {
	base := `{"viabilidade":"viável com ressalvas","resumo":"dá para fazer",
	          "riscos":["quebra o relatório antigo"],"dificuldades":["não há teste na área"],
	          "alternativas":%s}`
	three := `[{"nome":"a","descricao":"d","esforco":"baixo","recomendada":true},
	           {"nome":"b","descricao":"d","esforco":"médio","recomendada":false},
	           {"nome":"c","descricao":"d","esforco":"alto","recomendada":false}]`

	got, err := ParseStudy(fmt.Sprintf(base, three))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Feasibility != FeasibilityWithCaveat {
		t.Errorf("viabilidade %q", got.Feasibility)
	}
	if len(got.Alternatives) != 3 {
		t.Errorf("alternativas: %d", len(got.Alternatives))
	}
	rec, ok := got.Recommended()
	if !ok || rec.Name != "a" || rec.Effort != EffortLow {
		t.Errorf("recomendada: %+v (ok=%v)", rec, ok)
	}
	if len(got.Risks) != 1 || len(got.Difficulties) != 1 {
		t.Errorf("riscos/dificuldades perdidos: %#v %#v", got.Risks, got.Difficulties)
	}

	none := `[{"nome":"a","descricao":"d","esforco":"baixo","recomendada":false}]`
	if _, err := ParseStudy(fmt.Sprintf(base, none)); err == nil {
		t.Error("estudo sem recomendação deveria falhar")
	}
	two := `[{"nome":"a","descricao":"d","esforco":"baixo","recomendada":true},
	         {"nome":"b","descricao":"d","esforco":"alto","recomendada":true}]`
	if _, err := ParseStudy(fmt.Sprintf(base, two)); err == nil {
		t.Error("estudo com duas recomendações deveria falhar: o Go não tem como escolher")
	}
}

func TestParseStudyRejectsUnknownEnums(t *testing.T) {
	cases := map[string]string{
		"viabilidade": `{"viabilidade":"talvez","resumo":"r","alternativas":[{"nome":"a","descricao":"d","recomendada":true}]}`,
		"esforço":     `{"viabilidade":"viável","resumo":"r","alternativas":[{"nome":"a","descricao":"d","esforco":"gigante","recomendada":true}]}`,
		"sem resumo":  `{"viabilidade":"viável","resumo":" ","alternativas":[{"nome":"a","descricao":"d","recomendada":true}]}`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseStudy(reply); err == nil {
				t.Fatal("esperava falha")
			} else if domain.ClassifyAgentError(err) != domain.AgentErrMalformed {
				t.Errorf("classificou como %q", domain.ClassifyAgentError(err))
			}
		})
	}
}
