package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleAfter is how old a snapshot can be before report/printSimpleSummary calls it out.
// report is deliberately offline (reads only the snapshot), so this is the only signal that
// what you're looking at might not reflect Rancher's current state -- e.g. an annotation
// rollout finished after the last collect won't show up until you collect again.
const staleAfter = 24 * time.Hour

func collectedAtSummary(collectedAt string) string {
	if collectedAt == "" {
		return "unknown"
	}
	t, err := time.Parse(time.RFC3339, collectedAt)
	if err != nil {
		return collectedAt
	}
	age := time.Since(t)
	s := t.Local().Format("2006-01-02 15:04 MST") + fmt.Sprintf(" (%s ago)", age.Round(time.Minute))
	if age > staleAfter {
		s += " -- STALE: re-run collect before trusting this"
	}
	return s
}

func writeSimpleReports(rep *SimpleReport, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	steps := []struct {
		name string
		fn   func(io.Writer) error
	}{
		{"report.md", func(w io.Writer) error { return writeMarkdown(w, rep) }},
		{"rke1-inventory.csv", func(w io.Writer) error { return writeInventoryCSV(w, rep) }},
		{"found-in-rke2.csv", func(w io.Writer) error { return writeFoundCSV(w, rep) }},
		{"missing-in-rke2.csv", func(w io.Writer) error { return writeMissingCSV(w, rep) }},
	}
	for _, s := range steps {
		f, err := os.Create(filepath.Join(dir, s.name))
		if err != nil {
			return err
		}
		if err := s.fn(f); err != nil {
			f.Close()
			return fmt.Errorf("%s: %w", s.name, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func printSimpleSummary(w io.Writer, rep *SimpleReport) {
	env := rep.Env
	if env == "" {
		env = "(all)"
	}
	fmt.Fprintf(w, "\nsnapshot collected: %s\n", collectedAtSummary(rep.CollectedAt))
	fmt.Fprintf(w, "env=%s  rke1_hosts=%d  found_in_rke2=%d  missing_in_rke2=%d\n",
		env, len(rep.Inventory), len(rep.Found), len(rep.Missing))
	flagged := 0
	for _, r := range rep.Found {
		if r.Flag != "" {
			flagged++
		}
	}
	if flagged > 0 {
		fmt.Fprintf(w, "  of those found, %d are flagged as differing (namespace/gslb/ingress) from every RKE2 match\n", flagged)
	}
	withHint := 0
	for _, r := range rep.Missing {
		if len(r.LikelyRKE2) > 0 {
			withHint++
		}
	}
	fmt.Fprintf(w, "  of those missing, %d have a likely RKE2 cluster identified via the legacy-cluster annotation\n", withHint)
}

func md(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func joinOrDash(ss []string) string {
	if len(ss) == 0 {
		return "–"
	}
	return strings.Join(ss, ", ")
}

func writeMarkdown(w io.Writer, rep *SimpleReport) error {
	p := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
	env := rep.Env
	if env == "" {
		env = "all"
	}
	p("# GSLB inventory: RKE1 vs RKE2 (%s)\n\n", env)
	p("Snapshot collected: %s. This report is built entirely offline from that snapshot --\n", collectedAtSummary(rep.CollectedAt))
	p("changes in Rancher since then (e.g. a `%s` rollout) won't show up until you re-run `collect`.\n\n", "clusterpool/legacy-cluster")
	p("%d RKE1 hosts total — %d found in RKE2, %d missing.\n\n", len(rep.Inventory), len(rep.Found), len(rep.Missing))

	p("## 1. RKE1 inventory\n\n")
	p("Every GSLB-fronted host, per RKE1 cluster.\n\n")
	p("| Cluster | Namespace | GSLB | Host | Ingress |\n|---|---|---|---|---|\n")
	for _, r := range rep.Inventory {
		p("| %s | %s | %s | %s | %s |\n", md(r.Cluster), md(r.Namespace), md(r.Gslb), md(r.Host), md(orDash(r.Ingress)))
	}

	p("\n## 2. Found in RKE2\n\n")
	p("RKE1 hosts confirmed to also exist on RKE2, and where. `Designated` is where `naming.clusterMap`\n")
	p("says this RKE1 cluster's apps belong (blank when that base isn't in the map). `flag` is\n")
	p("`differs` when none of the RKE2 matches has the same namespace/GSLB/ingress name as RKE1,\n")
	p("and/or `elsewhere` when it was found only on clusters other than Designated.\n\n")
	p("| Cluster | Namespace | GSLB | Host | Ingress | Found on (RKE2) | Designated | Flag |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range rep.Found {
		p("| %s | %s | %s | %s | %s | %s | %s | %s |\n", md(r.Cluster), md(r.Namespace), md(r.Gslb), md(r.Host),
			md(orDash(r.Ingress)), md(joinOrDash(r.RKE2Clusters)), md(joinOrDash(r.Designated)), orDash(r.Flag))
	}

	p("\n## 3. Missing in RKE2\n\n")
	p("RKE1 hosts not found on any (nonprod) RKE2 cluster. `Likely RKE2 cluster` comes from\n")
	p("`naming.clusterMap` (the migration plan) when this RKE1 cluster's base is in it; otherwise\n")
	p("from any RKE2 cluster whose `clusterpool/legacy-cluster` annotation already points back at\n")
	p("it — left blank when neither knows, never guessed.\n\n")
	p("| Cluster | Namespace | GSLB | Host | Ingress | Likely RKE2 cluster |\n|---|---|---|---|---|---|\n")
	for _, r := range rep.Missing {
		p("| %s | %s | %s | %s | %s | %s |\n", md(r.Cluster), md(r.Namespace), md(r.Gslb), md(r.Host),
			md(orDash(r.Ingress)), md(joinOrDash(r.LikelyRKE2)))
	}
	return nil
}

func writeInventoryCSV(w io.Writer, rep *SimpleReport) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{"cluster", "namespace", "gslb", "host", "ingress"}); err != nil {
		return err
	}
	for _, r := range rep.Inventory {
		if err := cw.Write([]string{r.Cluster, r.Namespace, r.Gslb, r.Host, r.Ingress}); err != nil {
			return err
		}
	}
	return cw.Error()
}

func writeFoundCSV(w io.Writer, rep *SimpleReport) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{"cluster", "namespace", "gslb", "host", "ingress", "found_on_rke2", "designated", "flag"}); err != nil {
		return err
	}
	for _, r := range rep.Found {
		if err := cw.Write([]string{r.Cluster, r.Namespace, r.Gslb, r.Host, r.Ingress,
			strings.Join(r.RKE2Clusters, ";"), strings.Join(r.Designated, ";"), r.Flag}); err != nil {
			return err
		}
	}
	return cw.Error()
}

func writeMissingCSV(w io.Writer, rep *SimpleReport) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{"cluster", "namespace", "gslb", "host", "ingress", "likely_rke2_cluster"}); err != nil {
		return err
	}
	for _, r := range rep.Missing {
		if err := cw.Write([]string{r.Cluster, r.Namespace, r.Gslb, r.Host, r.Ingress, strings.Join(r.LikelyRKE2, ";")}); err != nil {
			return err
		}
	}
	return cw.Error()
}
