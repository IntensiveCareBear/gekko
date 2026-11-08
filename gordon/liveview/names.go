package main

import "hash/fnv"

// gordonFirstNames is the curated pool the deterministic name generator draws from.
// Any persona id maps to a stable entry, so "Firstname Gordon" names are consistent
// across restarts and auto-cover future personas without a registry edit.
var gordonFirstNames = []string{
	"Jebediah", "Mark", "Cornelius", "Wolfgang", "Baxter",
	"Percival", "Otis", "Rufus", "Thaddeus", "Silas",
	"Ambrose", "Horace", "Ignatius", "Barnaby", "Ezekiel",
	"Montgomery", "Reginald", "Cassius", "Leopold", "Fitzgerald",
}

// gordonName maps a persona id to a stable "Firstname Gordon" display name by hashing
// the id into the curated first-name pool. Deterministic: the same id always yields
// the same name. An operator override (config display_name) takes precedence upstream.
func gordonName(personaID string) string {
	if personaID == "" {
		return "Gordon"
	}
	// The weather desk isn't a Gordon personality (deterministic, no LLM), so it gets a
	// fixed themed name outside the hashed pool — also avoids a pool collision.
	if personaID == "weather" {
		return "Stormy Gordon"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(personaID))
	idx := int(h.Sum32()) % len(gordonFirstNames)
	if idx < 0 {
		idx += len(gordonFirstNames)
	}
	return gordonFirstNames[idx] + " Gordon"
}
