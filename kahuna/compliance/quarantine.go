package compliance

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gekko/kahuna/ingest"
)

type QuarantinedEvent struct {
	Event           *ingest.GekkoIngestEvent `json:"event"`
	Reason          string                   `json:"reason"`
	QuarantinedAtNS int64                    `json:"quarantined_at_ns"`
}

type Quarantiner struct {
	primaryDir  string
	fallbackDir string
}

func NewQuarantiner(primaryDir, fallbackDir string) *Quarantiner {
	return &Quarantiner{
		primaryDir:  primaryDir,
		fallbackDir: fallbackDir,
	}
}

// Quarantine logs and writes the dropped event to a quarantine warning JSON file.
func (q *Quarantiner) Quarantine(event *ingest.GekkoIngestEvent, reason string) error {
	qEvent := QuarantinedEvent{
		Event:           event,
		Reason:          reason,
		QuarantinedAtNS: time.Now().UnixNano(),
	}

	data, err := json.MarshalIndent(qEvent, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal quarantine event: %w", err)
	}

	filename := fmt.Sprintf("quarantine_%s_%s_%d.json", event.SourceID, event.PayloadHash, qEvent.QuarantinedAtNS)

	// Try writing to primary directory
	err = q.writeFile(q.primaryDir, filename, data)
	if err == nil {
		log.Printf("[WARNING] COMPLIANCE VIOLATION: Event quarantined at: %s/%s. Reason: %s", q.primaryDir, filename, reason)
		return nil
	}

	// Fallback if writing to primary fails (e.g. permission issues)
	log.Printf("[INFO] Primary quarantine directory (%s) unavailable: %v. Using fallback directory: %s", q.primaryDir, err, q.fallbackDir)
	fallbackErr := q.writeFile(q.fallbackDir, filename, data)
	if fallbackErr != nil {
		return fmt.Errorf("failed to write quarantine event to both primary (%v) and fallback (%v) dirs", err, fallbackErr)
	}

	log.Printf("[WARNING] COMPLIANCE VIOLATION (FALLBACK): Event quarantined at: %s/%s. Reason: %s", q.fallbackDir, filename, reason)
	return nil
}

func (q *Quarantiner) writeFile(dir, filename string, data []byte) error {
	// Ensure directory exists
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	path := filepath.Join(dir, filename)
	return os.WriteFile(path, data, 0644)
}
