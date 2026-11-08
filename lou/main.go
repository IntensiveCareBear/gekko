// Lou is Gekko's daily learning loop (docs/lou-learning-loop-proposal.md),
// phase 1: advisory only. It reads the runs/ archives, distills evidence-backed
// lessons about trading judgment, persists them cumulatively under runs/lou/,
// and emits a one-minute human digest. Zero write-back — Lou never touches
// config or code; suggested actions are for a human to consider.
//
// Usage: lou [-runs DIR] [-min-sample N]
// Cadence: run after each archived run or daily (cron / run.sh hook later).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	runsDir := flag.String("runs", "runs", "runs/ directory holding trade archives")
	minSample := flag.Int("min-sample", 25, "minimum trades before an analyzer may claim a pattern")
	autoTune := flag.Bool("autotune", false, "phase 3: apply validated lessons to whitelisted config knobs (paper)")
	doRollback := flag.Bool("rollback", false, "remove overrides.env, restoring every knob to its default at next launch")
	flag.Parse()

	if *doRollback {
		if err := rollback(filepath.Join(*runsDir, "lou"), time.Now()); err != nil {
			log.Fatalf("[ERROR] lou rollback: %v", err)
		}
		fmt.Println("overrides.env removed — next launch runs on defaults.")
		return
	}

	level := resolveAutonomy(filepath.Join(*runsDir, "lou"), *autoTune)
	log.Printf("[INFO] lou autonomy: %s", level)
	digest, err := pass(*runsDir, *minSample, level, time.Now())
	if err != nil {
		log.Fatalf("[ERROR] lou: %v", err)
	}
	fmt.Print(digest)
}

// pass executes one full learning pass: load → analyze → merge → (optional)
// auto-tune → persist → digest. Returns the digest markdown (also written
// under <runs>/lou/).
func pass(runsDir string, minSample int, level Autonomy, now time.Time) (string, error) {
	trades, runs, skipped, err := loadAllRuns(runsDir)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "", fmt.Errorf("no runs with trades.jsonl under %s", runsDir)
	}

	store, err := OpenStore(filepath.Join(runsDir, "lou"))
	if err != nil {
		return "", err
	}
	candidates := runAnalyzers(trades, minSample)
	changes := store.Merge(candidates, now)
	if err := store.Save(changes, now); err != nil {
		return "", err
	}

	tuned, err := autotune(filepath.Join(runsDir, "lou"), store.Lessons, now, level)
	if err != nil {
		return "", err
	}

	digest := buildDigest(now, runs, len(trades), skipped, store, changes, tuned, level)
	path := filepath.Join(runsDir, "lou", "digest-"+now.Format("20060102-150405")+".md")
	if err := os.WriteFile(path, []byte(digest), 0o644); err != nil {
		return "", err
	}
	return digest, nil
}

// buildDigest renders the "what I learned and what I changed" summary — short
// enough to read in a minute, ordered by what changed this pass.
func buildDigest(now time.Time, runs []string, trades, skipped int, store *Store, ch Changes, tuned []TuneChange, level Autonomy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Lou digest — %s\n\n", now.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "Corpus: %d closed trades across %d runs (%s … %s)", trades, len(runs), runs[0], runs[len(runs)-1])
	if skipped > 0 {
		fmt.Fprintf(&b, "; %d malformed lines skipped", skipped)
	}
	b.WriteString("\n")

	section := func(title string, ls []*Lesson) {
		if len(ls) == 0 {
			return
		}
		sort.Slice(ls, func(i, j int) bool { return ls[i].Confidence > ls[j].Confidence })
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		for _, l := range ls {
			fmt.Fprintf(&b, "- **%s** (confidence %.2f, %d trades across %d runs)\n",
				l.Pattern, l.Confidence, l.Evidence.Trades, len(l.Evidence.Runs))
			if l.SuggestedAction != "" {
				fmt.Fprintf(&b, "  - suggested: %s\n", l.SuggestedAction)
			}
			if l.RetiredNote != "" {
				fmt.Fprintf(&b, "  - retired: %s\n", l.RetiredNote)
			}
		}
	}
	section("New lessons (proposed)", ch.New)
	section("Promoted to validated", ch.Validated)
	section("Retired", ch.Retired)
	section("Reconfirmed", ch.Updated)

	if len(ch.New)+len(ch.Validated)+len(ch.Retired)+len(ch.Updated) == 0 {
		b.WriteString("\nNo pattern met the evidence thresholds this pass.\n")
	}

	// Standing advisory: everything currently active, for readers who only see
	// the latest digest.
	var active []*Lesson
	for _, l := range store.Lessons {
		if l.Status != "retired" {
			active = append(active, l)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Confidence > active[j].Confidence })
	fmt.Fprintf(&b, "\n## Active lesson book (%d)\n\n", len(active))
	for _, l := range active {
		fmt.Fprintf(&b, "- [%s] %s\n", l.Status, l.Pattern)
	}

	switch {
	case len(tuned) > 0:
		fmt.Fprintf(&b, "\n## What I changed (autonomy: %s, paper)\n\n", level)
		for _, c := range tuned {
			switch c.Action {
			case "bench":
				fmt.Fprintf(&b, "- **benched %s** (into shadow mode) — driven by %s (%q)\n", c.Target, c.LessonID, c.Pattern)
			case "blacklist":
				fmt.Fprintf(&b, "- **blacklisted family %s** — driven by %s (%q)\n", c.Target, c.LessonID, c.Pattern)
			default:
				fmt.Fprintf(&b, "- **%s: %.2f → %.2f** — driven by %s (%q)\n", c.Knob, c.Old, c.New, c.LessonID, c.Pattern)
			}
		}
		b.WriteString("\nTakes effect at the next launch (run.sh sources overrides.env). Roll back with `lou -rollback`.\n")
	case level.mayTune():
		fmt.Fprintf(&b, "\n_Autonomy %s: no validated lesson warranted a change this pass._\n", level)
	default:
		b.WriteString("\n_Autonomy observe: analysis only, no config was changed._\n")
	}
	return b.String()
}
