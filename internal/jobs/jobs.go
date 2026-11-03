package jobs

import "fmt"

// State machine for durable provisioning jobs.
var transitions = map[string]map[string]bool{
	"queued":      {"running": true, "cancelled": true, "failed": true, "interrupted": true},
	"running":     {"succeeded": true, "failed": true, "cancelled": true, "interrupted": true, "queued": true}, // queued = retry scheduled (bounded auto-retry)
	"interrupted": {"failed": true, "cancelled": true},
	"failed":      {},
	"succeeded":   {},
	"cancelled":   {},
}

func CanTransition(from, to string) bool {
	m, ok := transitions[from]
	if !ok {
		return false
	}
	return m[to]
}

func ValidateTransition(from, to string) error {
	if from == to {
		return nil
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid job state transition %s -> %s", from, to)
	}
	return nil
}

func IsTerminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled"
}

func IsActive(status string) bool {
	return status == "queued" || status == "running"
}

// Kinds allowlisted for the provisioning agent.
var Kinds = map[string]bool{
	"create_site": true, "delete_site": true, "start_site": true, "stop_site": true,
	"restart_site": true, "backup_site": true, "restore_site": true, "wpcli": true,
	"apply_config": true, "update_wordpress": true, "rotate_db_password": true,
	"repair_site": true, "import_site": true,
}

func ValidateKind(k string) error {
	if !Kinds[k] {
		return fmt.Errorf("job kind %q is not allowlisted", k)
	}
	return nil
}
