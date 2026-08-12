package prompts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// The envelope is a single JSON object, and it is parsed strictly. A reply that does
// not fit is a domain.AgentErrMalformed, which Retryable() says NOT to retry: the
// same prompt produces the same reply, so a second attempt buys nothing and spends
// the subscription. It becomes an unanalysed issue instead — never a silence.

// Triage is the Triagem's verdict plus the reasoning behind it. The rationale is
// required because auditing the Triagem later is the whole point of recording the
// discarded Ocorrências.
type Triage struct {
	Verdict   domain.Verdict
	Rationale string
}

// SuspectFile is a real path in the analysed repository. "Real" is the requirement:
// the predecessor described symptoms and never named a file, which left the
// investigation with the human.
type SuspectFile struct {
	Path string
	// Line is 0 when the agent could not pin one down.
	Line int
	Why  string
}

// Confidence is how much the Diagnóstico believes itself.
type Confidence string

const (
	ConfidenceHigh   Confidence = "alta"
	ConfidenceMedium Confidence = "média"
	ConfidenceLow    Confidence = "baixa"
)

// Diagnosis is what the long session produces for a defeito.
type Diagnosis struct {
	ProbableCause string
	SuspectFiles  []SuspectFile
	Evidence      []string
	Confidence    Confidence
	Adjustment    Adjustment
}

// Feasibility is the Estudo's answer to "can this be done".
type Feasibility string

const (
	FeasibilityYes        Feasibility = "viável"
	FeasibilityWithCaveat Feasibility = "viável com ressalvas"
	FeasibilityNo         Feasibility = "inviável"
)

// Effort is the coarse cost of an alternative.
type Effort string

const (
	EffortLow    Effort = "baixo"
	EffortMedium Effort = "médio"
	EffortHigh   Effort = "alto"
)

// Alternative is one of the paths the Estudo found.
type Alternative struct {
	Name        string
	Description string
	Effort      Effort
	Recommended bool
}

// Study is what the long session produces for a sugestão.
type Study struct {
	Feasibility  Feasibility
	Summary      string
	Alternatives []Alternative
	Risks        []string
	Difficulties []string
	Adjustment   Adjustment
}

// Recommended returns the single recommended alternative. Parsing guarantees there is
// exactly one, so the boolean is only false on a zero-value Study.
func (s Study) Recommended() (Alternative, bool) {
	for _, a := range s.Alternatives {
		if a.Recommended {
			return a, true
		}
	}
	return Alternative{}, false
}

// AdjustDirection is the Prioridade move the agent may ask for.
type AdjustDirection string

const (
	AdjustKeep AdjustDirection = "manter"
	AdjustUp   AdjustDirection = "subir"
	AdjustDown AdjustDirection = "descer"
)

// Adjustment is the agent's judgement on top of the deterministic Prioridade rule.
// It moves at most one level, and never without a written justification.
type Adjustment struct {
	Direction AdjustDirection
	Rationale string
}

// Apply moves p by the adjustment. The one-level ceiling is enforced here, in Go,
// rather than trusted from the reply: Raise and Lower saturate at the automatic
// ceiling and floor, so an agent asking to escalate a 2 — High cannot reach Urgent.
func (a Adjustment) Apply(p domain.Priority) domain.Priority {
	switch a.Direction {
	case AdjustUp:
		return p.Raise(1)
	case AdjustDown:
		return p.Lower(1)
	}
	return p
}

// ParseTriage reads the Triagem envelope.
func ParseTriage(reply string) (Triage, error) {
	raw, err := extractEnvelope(reply)
	if err != nil {
		return Triage{}, err
	}
	var env struct {
		Desfecho      string `json:"desfecho"`
		Justificativa string `json:"justificativa"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Triage{}, malformed("JSON inválido na resposta da triagem: %v", err)
	}
	verdict, err := domain.ParseVerdict(env.Desfecho)
	if err != nil {
		return Triage{}, malformed("%v", err)
	}
	rationale := strings.TrimSpace(env.Justificativa)
	if rationale == "" {
		return Triage{}, malformed("triagem sem justificativa: o desfecho %q não é auditável sozinho", string(verdict))
	}
	return Triage{Verdict: verdict, Rationale: rationale}, nil
}

// ParseDiagnosis reads the Diagnóstico envelope. At least one suspect file is
// mandatory: a diagnosis that names no path is the predecessor's failure with better
// formatting, and accepting it would put "causa provável: o pedido falhou" on the
// board as if it were work.
func ParseDiagnosis(reply string) (Diagnosis, error) {
	raw, err := extractEnvelope(reply)
	if err != nil {
		return Diagnosis{}, err
	}
	var env struct {
		CausaProvavel     string `json:"causa_provavel"`
		ArquivosSuspeitos []struct {
			Caminho string `json:"caminho"`
			Linha   int    `json:"linha"`
			Porque  string `json:"porque"`
		} `json:"arquivos_suspeitos"`
		Evidencias []string            `json:"evidencias"`
		Confianca  string              `json:"confianca"`
		Ajuste     *adjustmentEnvelope `json:"ajuste_prioridade"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Diagnosis{}, malformed("JSON inválido na resposta do diagnóstico: %v", err)
	}

	cause := strings.TrimSpace(env.CausaProvavel)
	if cause == "" {
		return Diagnosis{}, malformed("diagnóstico sem causa provável")
	}

	var files []SuspectFile
	for _, f := range env.ArquivosSuspeitos {
		path := strings.TrimSpace(f.Caminho)
		if path == "" {
			continue
		}
		line := f.Linha
		if line < 0 {
			line = 0
		}
		files = append(files, SuspectFile{Path: path, Line: line, Why: strings.TrimSpace(f.Porque)})
	}
	if len(files) == 0 {
		return Diagnosis{}, malformed("diagnóstico sem arquivo suspeito: descrever o sintoma sem apontar código é a falha que este sistema existe para não repetir")
	}

	confidence := ConfidenceMedium
	if s := strings.TrimSpace(env.Confianca); s != "" {
		switch deaccent(s) {
		case "alta":
			confidence = ConfidenceHigh
		case "media":
			confidence = ConfidenceMedium
		case "baixa":
			confidence = ConfidenceLow
		default:
			return Diagnosis{}, malformed("confiança desconhecida: %q", s)
		}
	}

	adj, err := parseAdjustment(env.Ajuste)
	if err != nil {
		return Diagnosis{}, err
	}

	return Diagnosis{
		ProbableCause: cause,
		SuspectFiles:  files,
		Evidence:      trimAll(env.Evidencias),
		Confidence:    confidence,
		Adjustment:    adj,
	}, nil
}

// ParseStudy reads the Estudo envelope.
//
// Exactly one recommended alternative is mandatory — with zero the Estudo answers
// nothing and with two Go has no way to pick, and picking wrong is worse than
// failing. The count of alternatives is NOT enforced: the prompt asks for three, but
// throwing away an otherwise complete study because it produced two is a bad trade,
// since a malformed reply is never retried and ends up as an unanalysed issue.
func ParseStudy(reply string) (Study, error) {
	raw, err := extractEnvelope(reply)
	if err != nil {
		return Study{}, err
	}
	var env struct {
		Viabilidade  string `json:"viabilidade"`
		Resumo       string `json:"resumo"`
		Alternativas []struct {
			Nome        string `json:"nome"`
			Descricao   string `json:"descricao"`
			Esforco     string `json:"esforco"`
			Recomendada bool   `json:"recomendada"`
		} `json:"alternativas"`
		Riscos       []string            `json:"riscos"`
		Dificuldades []string            `json:"dificuldades"`
		Ajuste       *adjustmentEnvelope `json:"ajuste_prioridade"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Study{}, malformed("JSON inválido na resposta do estudo: %v", err)
	}

	var feasibility Feasibility
	switch collapse(deaccent(env.Viabilidade)) {
	case "viavel":
		feasibility = FeasibilityYes
	case "viavel com ressalvas":
		feasibility = FeasibilityWithCaveat
	case "inviavel":
		feasibility = FeasibilityNo
	case "":
		return Study{}, malformed("estudo sem viabilidade")
	default:
		return Study{}, malformed("viabilidade desconhecida: %q", env.Viabilidade)
	}

	summary := strings.TrimSpace(env.Resumo)
	if summary == "" {
		return Study{}, malformed("estudo sem resumo")
	}

	var alts []Alternative
	recommended := 0
	for _, a := range env.Alternativas {
		name := strings.TrimSpace(a.Nome)
		desc := strings.TrimSpace(a.Descricao)
		if name == "" && desc == "" {
			continue
		}
		effort, err := parseEffort(a.Esforco)
		if err != nil {
			return Study{}, err
		}
		if a.Recomendada {
			recommended++
		}
		alts = append(alts, Alternative{Name: name, Description: desc, Effort: effort, Recommended: a.Recomendada})
	}
	if len(alts) == 0 {
		return Study{}, malformed("estudo sem alternativa nenhuma")
	}
	if recommended != 1 {
		return Study{}, malformed("o estudo precisa de exatamente uma alternativa recomendada, veio %d", recommended)
	}

	adj, err := parseAdjustment(env.Ajuste)
	if err != nil {
		return Study{}, err
	}

	return Study{
		Feasibility:  feasibility,
		Summary:      summary,
		Alternatives: alts,
		Risks:        trimAll(env.Riscos),
		Difficulties: trimAll(env.Dificuldades),
		Adjustment:   adj,
	}, nil
}

// CheckPaths returns the suspect paths that do not exist under root. It is how "name
// a real repository path" stops being a request in the prompt and becomes something
// the Go side can see. It is not a security check — the clone is mounted read-only
// and the perimeter is the container.
func CheckPaths(root string, files []SuspectFile) []string {
	var missing []string
	for _, f := range files {
		clean := filepath.Clean(strings.TrimSpace(f.Path))
		switch {
		case clean == "" || clean == "." || filepath.IsAbs(clean):
			missing = append(missing, f.Path)
			continue
		case clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)):
			// A path escaping the clone is by definition not a file of the project.
			missing = append(missing, f.Path)
			continue
		}
		if _, err := os.Stat(filepath.Join(root, clean)); err != nil {
			missing = append(missing, f.Path)
		}
	}
	return missing
}

type adjustmentEnvelope struct {
	Direcao       string `json:"direcao"`
	Justificativa string `json:"justificativa"`
}

// parseAdjustment turns the optional adjustment into a decision.
//
// An adjustment with no written justification is DROPPED rather than treated as a
// malformed reply. The PRD admits the agent's judgement "apenas com justificativa
// escrita", so an unjustified move is simply not an adjustment — and failing the
// whole Diagnóstico over a missing sentence would throw away the investigation to
// punish the formatting.
func parseAdjustment(env *adjustmentEnvelope) (Adjustment, error) {
	if env == nil {
		return Adjustment{Direction: AdjustKeep}, nil
	}
	var dir AdjustDirection
	switch deaccent(env.Direcao) {
	case "", "manter":
		return Adjustment{Direction: AdjustKeep}, nil
	case "subir":
		dir = AdjustUp
	case "descer":
		dir = AdjustDown
	default:
		return Adjustment{}, malformed("direção de ajuste desconhecida: %q", env.Direcao)
	}
	rationale := strings.TrimSpace(env.Justificativa)
	if rationale == "" {
		return Adjustment{Direction: AdjustKeep}, nil
	}
	return Adjustment{Direction: dir, Rationale: rationale}, nil
}

func parseEffort(s string) (Effort, error) {
	switch deaccent(s) {
	case "", "medio":
		return EffortMedium, nil
	case "baixo":
		return EffortLow, nil
	case "alto":
		return EffortHigh, nil
	}
	return "", malformed("esforço desconhecido: %q", s)
}

// extractEnvelope finds the JSON object inside a reply. The prompt asks for the
// object and nothing else; in practice the one deviation that shows up constantly is
// a markdown fence around it, so that single case is absorbed and everything else is
// malformed. Being generous here — hunting for the first `{` in a wall of prose —
// would mean sometimes parsing an example the model wrote while thinking out loud.
func extractEnvelope(reply string) ([]byte, error) {
	s := strings.TrimSpace(reply)
	if s == "" {
		return nil, malformed("resposta vazia")
	}
	if inner, ok := lastFencedBlock(s); ok {
		s = strings.TrimSpace(inner)
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, malformed("esperava um único objeto JSON, veio: %s", excerpt(s))
	}
	return []byte(s), nil
}

// lastFencedBlock returns the contents of the last ```-fenced block. The last one
// because a model that explains itself puts the answer at the end.
func lastFencedBlock(s string) (string, bool) {
	const fence = "```"
	end := strings.LastIndex(s, fence)
	if end < 0 {
		return "", false
	}
	start := strings.LastIndex(s[:end], fence)
	if start < 0 {
		return "", false
	}
	body := s[start+len(fence) : end]
	// Drop the language tag that follows the opening fence ("```json").
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		if tag := strings.TrimSpace(body[:i]); tag == "" || isLanguageTag(tag) {
			body = body[i+1:]
		}
	}
	return body, true
}

func isLanguageTag(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '+') {
			return false
		}
	}
	return true
}

func malformed(format string, a ...any) error {
	return &domain.AgentError{Kind: domain.AgentErrMalformed, Detail: fmt.Sprintf(format, a...)}
}

func trimAll(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// deaccent lowercases and strips the accents the model may or may not type. The
// canonical values keep their accents — they are contractual and reach SQLite and the
// command line — but rejecting "media" for "média" would fail an analysis over a
// keyboard.
var accents = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a",
	"é", "e", "ê", "e",
	"í", "i",
	"ó", "o", "ô", "o", "õ", "o",
	"ú", "u", "ü", "u",
	"ç", "c",
)

func deaccent(s string) string {
	return accents.Replace(strings.ToLower(strings.TrimSpace(s)))
}

// collapse squeezes runs of whitespace, so "viável  com ressalvas" still matches.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func excerpt(s string) string {
	const max = 160
	r := []rune(collapse(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
