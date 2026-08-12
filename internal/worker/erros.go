package worker

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// destinoRetry is how long an unreachable Destino is given before the next try, when
// the error itself does not say. Short, because everything behind it is waiting.
const destinoRetry = time.Minute

// destinoStuck is the wait for a Destino that will not fix itself — a credential the
// Linear API refuses. Retrying that every minute forever only fills the log, and the
// item still must not leave the queue.
const destinoStuck = 15 * time.Minute

// retryLater is the Análise asking to come back to the queue after a wait. It is the
// only way out of Analyse other than an ending: the Pool never closes a queue item
// while holding an error, so an Ocorrência cannot leave through here.
type retryLater struct {
	after time.Duration
	err   error
}

func (e *retryLater) Error() string {
	return fmt.Sprintf("volta para a fila em %s: %v", e.after, e.err)
}

func (e *retryLater) Unwrap() error { return e.err }

// destinoFailed is the one failure that must NOT end the Análise.
//
// Everything else has somewhere to be recorded — a failed session becomes an issue
// marked as unanalysed. A Destino that cannot be reached has nowhere: dropping the
// item there is exactly how an Ocorrência vanishes, which is the defect that killed
// the predecessor. So it goes back to the queue, however many times it takes.
//
// The wait comes from the error when the error knows: the Linear rate limit arrives
// as HTTP 400 and carries the instant the quota returns, and a client that reads only
// the status code treats it as permanent and gives up in silence.
func destinoFailed(op string, err error) error {
	wait := tracker.RetryAfter(err)
	switch {
	case wait > 0:
	case tracker.IsRetryable(err):
		wait = destinoRetry
	default:
		wait = destinoStuck
	}
	return &retryLater{after: wait, err: fmt.Errorf("%s: %w", op, err)}
}

// detailOf is the human sentence that goes onto the failure issue as the detail. The
// classification is already carried separately as the FailureKind, so repeating it
// here would print "timeout: timeout" on the board.
func detailOf(err error) string {
	var ae *domain.AgentError
	if errors.As(err, &ae) && strings.TrimSpace(ae.Detail) != "" {
		return ae.Detail
	}
	return err.Error()
}
