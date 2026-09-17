package main

import (
	"sort"
	"strings"
)

// ---- simple report model ----
//
// Three flat, independent lists -- no app/group abstraction, no per-DC slot matrix, no
// field-by-field diffing. A "cluster" is exactly one RKE1 or RKE2 cluster; env/dc are
// already part of its name so grouping by cluster is grouping by DC for free.

// InventoryRow is one GSLB-fronted host on one RKE1 cluster.
type InventoryRow struct {
	Cluster   string
	Namespace string
	Gslb      string
	Host      string
	Ingress   string
}

// FoundRow is an RKE1 host confirmed to also exist on at least one RKE2 cluster.
type FoundRow struct {
	Cluster      string   // RKE1 cluster
	Namespace    string   // RKE1 namespace
	Gslb         string   // RKE1 gslb name
	Host         string
	Ingress      string   // RKE1 ingress name
	RKE2Clusters []string // every RKE2 cluster the host was found on (sorted, deduped)
	Designated   []string // where the migration plan (clusterMap) says this RKE1 cluster's apps should land
	// Flag: "differs" when none of the RKE2 hits has the same namespace/gslb/ingress;
	// "elsewhere" when it was found only on clusters outside Designated; both joined by ";".
	Flag string
}

// MissingRow is an RKE1 host not found on any RKE2 cluster.
type MissingRow struct {
	Cluster   string
	Namespace string
	Gslb      string
	Host      string
	Ingress   string
	// LikelyRKE2: the clusterMap designation for this RKE1 cluster (its own env + dc). Falls
	// back to RKE2 clusters whose clusterpool/legacy-cluster hint points at it when the base
	// isn't in clusterMap. Empty only when neither knows -- never guessed.
	LikelyRKE2 []string
}

type SimpleReport struct {
	Env         string // "nonprod", "prod", or "" for no env filter -- both DCs are always included
	CollectedAt string // snap.CollectedAt, verbatim -- report is offline/snapshot-based, so this is
	// the only way to tell whether it reflects Rancher's current state or a stale collect.
	Inventory []InventoryRow
	Found     []FoundRow
	Missing   []MissingRow
}

// inEnv reports whether slot ("env/dc") belongs to env. slot == "" (unparseable cluster
// name) never matches, even when env == "", since we can't be sure what it is.
func inEnv(slot, env string) bool {
	if slot == "" {
		return false
	}
	if env == "" {
		return true
	}
	return strings.HasPrefix(slot, env+"/")
}

type targetHit struct {
	Cluster   string
	Namespace string
	Gslb      string
	Ingress   string
}

// hintMatchesBase reports whether the clusterpool/legacy-cluster annotation value(s) in hint
// refer to base (an RKE1 cluster's parsed base name, e.g. "cib-fx"). Checks
// naming.legacyAliases first (for renames with no textual relationship to base, e.g. "bolt"
// for "avaf"), then falls back to a suffix heuristic that tolerates the hint dropping the
// prefix and/or the -<env>-<dc> suffix. Comma/space separated lists in hint are all checked.
func hintMatchesBase(cfg *Config, hint, base string) bool {
	if hint == "" || base == "" {
		return false
	}
	for _, h := range strings.FieldsFunc(strings.ToLower(hint), func(r rune) bool { return r == ',' || r == ' ' }) {
		if cfg.Naming.LegacyAliases[h] == base {
			return true
		}
		if h == base || strings.HasSuffix(base, "-"+h) {
			return true
		}
		if b2, _, ok := cfg.parseLegacy(h); ok && (b2 == base || strings.HasSuffix(base, "-"+b2)) {
			return true
		}
	}
	return false
}

// buildSimpleReport walks the snapshot once and produces the three flat lists. env scopes
// both sides ("nonprod" keeps nonprod/270 + nonprod/sdc, "" keeps everything).
func buildSimpleReport(cfg *Config, snap *Snapshot, env string) *SimpleReport {
	rep := &SimpleReport{Env: env, CollectedAt: snap.CollectedAt}

	// Index every RKE2 host, across every target cluster in scope, regardless of pool/DC.
	targetHosts := map[string][]targetHit{}
	seen := map[string]map[string]bool{} // host -> "cluster|ns|gslb|ingress" -> true, de-dupes repeats
	for i := range snap.Clusters {
		c := &snap.Clusters[i]
		if c.Role != "target" {
			continue
		}
		if !inEnv(cfg.targetSlot(c.Endpoint, c.Name), env) {
			continue
		}
		for _, g := range c.Gslbs {
			for _, h := range g.Hosts {
				host := cfg.rewriteHost(h.Host) // lower-cased, trailing dot stripped -- matches the RKE1 side's key
				hit := targetHit{Cluster: c.Name, Namespace: g.Namespace, Gslb: g.Name, Ingress: h.Ingress}
				id := hit.Cluster + "|" + hit.Namespace + "|" + hit.Gslb + "|" + hit.Ingress
				if seen[host] == nil {
					seen[host] = map[string]bool{}
				}
				if seen[host][id] {
					continue
				}
				seen[host][id] = true
				targetHosts[host] = append(targetHosts[host], hit)
			}
		}
	}

	for i := range snap.Clusters {
		c := &snap.Clusters[i]
		if c.Role != "legacy" {
			continue
		}
		base, slot, ok := cfg.parseLegacy(c.Name)
		if !ok || !inEnv(slot, env) {
			continue
		}
		// The plan's designation for this exact RKE1 cluster (its own env + dc), e.g.
		// cib-corp-nonprod-270 -> subnp270-cap-4. Computed once per cluster, not per host.
		designated := designatedTargets(cfg, base, slot)
		likely := designated
		if len(likely) == 0 {
			likely = likelyTargets(cfg, snap, base, env) // annotation fallback for bases not in clusterMap
		}
		for _, g := range c.Gslbs {
			for _, h := range g.Hosts {
				inv := InventoryRow{Cluster: c.Name, Namespace: g.Namespace, Gslb: g.Name, Host: h.Host, Ingress: h.Ingress}
				rep.Inventory = append(rep.Inventory, inv)

				hits := targetHosts[cfg.rewriteHost(h.Host)]
				if len(hits) == 0 {
					rep.Missing = append(rep.Missing, MissingRow{
						Cluster: inv.Cluster, Namespace: inv.Namespace, Gslb: inv.Gslb, Host: inv.Host, Ingress: inv.Ingress,
						LikelyRKE2: likely,
					})
					continue
				}
				found := FoundRow{
					Cluster: inv.Cluster, Namespace: inv.Namespace, Gslb: inv.Gslb, Host: inv.Host, Ingress: inv.Ingress,
					Designated: designated,
				}
				exact, onDesignated := false, false
				for _, hit := range hits {
					found.RKE2Clusters = append(found.RKE2Clusters, hit.Cluster)
					if strings.EqualFold(hit.Namespace, inv.Namespace) && strings.EqualFold(hit.Gslb, inv.Gslb) && strings.EqualFold(hit.Ingress, inv.Ingress) {
						exact = true
					}
					if contains(designated, hit.Cluster) {
						onDesignated = true
					}
				}
				var flags []string
				if !exact {
					flags = append(flags, "differs")
				}
				if len(designated) > 0 && !onDesignated {
					flags = append(flags, "elsewhere")
				}
				found.Flag = strings.Join(flags, ";")
				found.RKE2Clusters = dedupeSorted(found.RKE2Clusters)
				rep.Found = append(rep.Found, found)
			}
		}
	}

	sort.Slice(rep.Inventory, func(i, j int) bool { return lessRow(rep.Inventory[i].Cluster, rep.Inventory[i].Host, rep.Inventory[j].Cluster, rep.Inventory[j].Host) })
	sort.Slice(rep.Found, func(i, j int) bool { return lessRow(rep.Found[i].Cluster, rep.Found[i].Host, rep.Found[j].Cluster, rep.Found[j].Host) })
	sort.Slice(rep.Missing, func(i, j int) bool { return lessRow(rep.Missing[i].Cluster, rep.Missing[i].Host, rep.Missing[j].Cluster, rep.Missing[j].Host) })
	return rep
}

// designatedTargets expands cfg.Naming.ClusterMap[base]'s patterns against this specific RKE1
// cluster's own env/dc (from slot, "env/dc"), e.g. base "cib-corp", slot "nonprod/270",
// pattern "sub{env}{dc}-cap-4" -> "subnp270-cap-4". This is the authoritative migration
// plan, straight from a human-maintained table -- it does not depend on whether Rancher's
// clusterpool/legacy-cluster annotation has been applied yet, or on any naming heuristic.
// Returns nil when base isn't in the map, so callers know to fall back to likelyTargets.
func designatedTargets(cfg *Config, base, slot string) []string {
	patterns := cfg.Naming.ClusterMap[strings.ToLower(base)]
	if len(patterns) == 0 {
		return nil
	}
	parts := strings.SplitN(slot, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	env, dc := parts[0], parts[1]
	if tok, ok := cfg.Naming.EnvTokens[env]; ok {
		env = tok
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range patterns {
		name := expandPattern(p, env, dc)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// expandPattern substitutes env/dc placeholders. Both {curly} and <angle> forms are
// accepted (the plan uses "sub<EV><DC>-cap-1" in one table and "ssm{env}{dc}-cap-0" in
// another); config normalisation already lower-cases patterns, so only lower-case tokens
// need handling here. A pattern with no placeholders is returned unchanged (a literal,
// slot-specific name), which is only correct if that RKE1 base has exactly one slot.
func expandPattern(pattern, env, dc string) string {
	r := strings.NewReplacer(
		"{env}", env, "{ev}", env, "{dc}", dc,
		"<env>", env, "<ev>", env, "<dc>", dc,
	)
	return r.Replace(pattern)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// likelyTargets returns the in-scope RKE2 clusters whose legacy-cluster annotation points
// at base, sorted. Fallback for bases with no clusterMap entry; empty when nothing is
// annotated for it either -- never guessed.
func likelyTargets(cfg *Config, snap *Snapshot, base, env string) []string {
	var out []string
	for i := range snap.Clusters {
		c := &snap.Clusters[i]
		if c.Role != "target" {
			continue
		}
		if !inEnv(cfg.targetSlot(c.Endpoint, c.Name), env) {
			continue
		}
		if hintMatchesBase(cfg, c.LegacyHint, base) {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

func dedupeSorted(ss []string) []string {
	sort.Strings(ss)
	out := ss[:0]
	for i, s := range ss {
		if i == 0 || ss[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func lessRow(clusterA, hostA, clusterB, hostB string) bool {
	if clusterA != clusterB {
		return clusterA < clusterB
	}
	return hostA < hostB
}
