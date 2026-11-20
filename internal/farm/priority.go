package farm

import "fmt"

// HandBrake process priorities on a worker. Low keeps a desktop or media
// server responsive while costing a dedicated encoder nothing; idle only uses
// otherwise unused CPU and disk time.
const (
	PriorityNormal = "normal"
	PriorityLow    = "low"
	PriorityIdle   = "idle"
)

// DefaultPriority is used when worker.priority is not set.
const DefaultPriority = PriorityLow

func ValidatePriority(p string) error {
	switch p {
	case "", PriorityNormal, PriorityLow, PriorityIdle:
		return nil
	}
	return fmt.Errorf("priority must be normal, low or idle, not %q", p)
}

// niceness is the Unix nice value for a priority.
func niceness(p string) int {
	switch p {
	case PriorityLow:
		return 10
	case PriorityIdle:
		return 19
	}
	return 0
}
