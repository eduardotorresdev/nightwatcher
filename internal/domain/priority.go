package domain

import "fmt"

// Priority is the Linear priority level. The scale is INVERTED and `0` sits outside
// the order (0=none, 1=Urgent, 4=Low), so every rule over it is a literal table and
// never arithmetic: `p - 1` produces the opposite of what it reads like.
type Priority int

const (
	PriorityNone   Priority = 0
	PriorityUrgent Priority = 1
	PriorityHigh   Priority = 2
	PriorityMedium Priority = 3
	PriorityLow    Priority = 4
)

// AutoCeiling is the highest priority the system ever produces on its own.
// `1 — Urgent` fires an urgent e-mail plus a 24h SLA; one false positive costs trust
// in the whole system, and the predecessor died of exactly this noise. Urgent is a
// human act.
const AutoCeiling = PriorityHigh

// AutoFloor is the lowest priority produced. `0 — No priority` is never produced:
// an absent priority is indistinguishable from a bug in the rule, and the board
// loses its ordering.
const AutoFloor = PriorityLow

func (p Priority) String() string {
	switch p {
	case PriorityNone:
		return "0 — No priority"
	case PriorityUrgent:
		return "1 — Urgent"
	case PriorityHigh:
		return "2 — High"
	case PriorityMedium:
		return "3 — Medium"
	case PriorityLow:
		return "4 — Low"
	}
	return fmt.Sprintf("prioridade inválida (%d)", int(p))
}

// Raise moves the priority up by n levels, saturating at AutoCeiling. Because the
// scale is inverted, moving *up* in urgency means moving *down* in number — which is
// precisely why this lives behind a named method instead of at every call site.
func (p Priority) Raise(n int) Priority {
	out := p
	for i := 0; i < n; i++ {
		if out <= AutoCeiling {
			return AutoCeiling
		}
		out--
	}
	if out < AutoCeiling {
		return AutoCeiling
	}
	return out
}

// Lower moves the priority down by n levels, saturating at AutoFloor.
func (p Priority) Lower(n int) Priority {
	out := p
	for i := 0; i < n; i++ {
		if out >= AutoFloor {
			return AutoFloor
		}
		out++
	}
	if out > AutoFloor {
		return AutoFloor
	}
	return out
}

// MoreUrgentThan reports whether p outranks q, reading through the inverted scale.
// PriorityNone is treated as the least urgent value there is.
func (p Priority) MoreUrgentThan(q Priority) bool {
	if p == PriorityNone {
		return false
	}
	if q == PriorityNone {
		return true
	}
	return p < q
}
