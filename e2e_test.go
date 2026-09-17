package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRancher struct {
	clusters []map[string]any
	objs     map[string][]map[string]any // "<clusterID><apipath>" -> items
}

func (f *fakeRancher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.URL.Path == "/v3/clusters" {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": f.clusters})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/k8s/clusters/")
	items, ok := f.objs[rest]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{}, "items": items})
}

func ing(ns, name, host, svc string, port int, owner map[string]any) map[string]any {
	meta := map[string]any{"name": name, "namespace": ns}
	if owner != nil {
		meta["ownerReferences"] = []any{owner}
	}
	return map[string]any{
		"metadata": meta,
		"spec": map[string]any{
			"ingressClassName": "nginx",
			"tls":              []any{map[string]any{"hosts": []any{"*.np.absa.africa", "web.absa.africa"}, "secretName": "wild"}},
			"rules": []any{map[string]any{"host": host, "http": map[string]any{"paths": []any{
				map[string]any{"path": "/", "pathType": "Prefix", "backend": map[string]any{"service": map[string]any{"name": svc, "port": map[string]any{"number": port}}}},
			}}}},
		},
	}
}

func gslbEmbedded(ns, name, host, strategy string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"ingress":  map[string]any{"rules": []any{map[string]any{"host": host}}},
			"strategy": map[string]any{"type": strategy, "dnsTtlSeconds": 30},
		},
		"status": map[string]any{"hosts": host, "serviceHealth": map[string]any{host: "Healthy"}},
	}
}

func gslbRef(ns, name, host, ingName string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"resourceRef": map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "name": ingName},
			"strategy":    map[string]any{"type": "roundRobin", "dnsTtlSeconds": 30},
		},
		"status": map[string]any{"hosts": host},
	}
}

func owner(name string) map[string]any {
	return map[string]any{"apiVersion": "k8gb.absa.oss/v1beta1", "kind": "Gslb", "name": name}
}

func cl(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "state": "active"}
}

const gp, ip = "/apis/k8gb.absa.oss/v1beta1/gslbs", "/apis/networking.k8s.io/v1/ingresses"

// TestSimpleReportEndToEnd exercises the flat inventory/found/missing model end to end
// against fake Rancher servers: two RKE1 clusters (nonprod/270 + nonprod/sdc) of the same
// app, one prod RKE1 cluster that must be excluded by the env filter, an unparseable
// cluster that must be silently skipped, and two RKE2 clusters holding a mix of exact
// matches, a same-host-different-namespace match (must be flagged), and nothing at all
// for one host (must land in missing, with its legacy-cluster hint resolved).
func TestSimpleReportEndToEnd(t *testing.T) {
	legacy := &fakeRancher{
		clusters: []map[string]any{
			cl("local", "local"),
			cl("c-1", "cib-corp-nonprod-270"),
			cl("c-2", "cib-corp-nonprod-sdc"),
			cl("c-3", "cib-corp-prod-270"),
			cl("c-4", "weird"),
		},
		objs: map[string][]map[string]any{
			"c-1" + gp: {
				gslbEmbedded("shop", "web", "web.np.absa.africa", "roundRobin"),
				gslbEmbedded("shop", "api-gslb", "api.np.absa.africa", "roundRobin"),
			},
			"c-1" + ip: {
				ing("shop", "web", "web.np.absa.africa", "web", 8080, owner("web")),
				ing("shop", "api-ing", "api.np.absa.africa", "api", 8080, owner("api-gslb")),
			},
			"c-2" + gp: {
				gslbEmbedded("shop", "web", "web.np.absa.africa", "roundRobin"),
				gslbEmbedded("shop", "batch", "batch.np.absa.africa", "roundRobin"),
			},
			"c-2" + ip: {
				ing("shop", "web", "web.np.absa.africa", "web", 8080, owner("web")),
				ing("shop", "batch", "batch.np.absa.africa", "batch", 80, owner("batch")),
			},
			// prod -- must be excluded entirely when env=nonprod.
			"c-3" + gp: {gslbEmbedded("shop", "web", "web.absa.africa", "failover")},
			"c-3" + ip: {ing("shop", "web", "web.absa.africa", "web", 8080, owner("web"))},
		},
	}
	target := &fakeRancher{
		clusters: []map[string]any{cl("c-m-1", "adonp270-cap-1"), cl("c-m-2", "subnpsdc-cap-2")},
		objs: map[string][]map[string]any{
			"c-m-1" + gp: {
				gslbEmbedded("shop", "web", "web.np.absa.africa", "roundRobin"),
				// same host, different namespace than RKE1 -> found, but flagged.
				gslbEmbedded("other", "api-gslb", "api.np.absa.africa", "roundRobin"),
			},
			"c-m-1" + ip: {
				ing("shop", "web", "web.np.absa.africa", "web", 8080, owner("web")),
				ing("other", "api-ing", "api.np.absa.africa", "api", 9090, owner("api-gslb")),
			},
			"c-m-2" + gp: {gslbEmbedded("shop", "web", "web.np.absa.africa", "roundRobin")},
			"c-m-2" + ip: {ing("shop", "web", "web.np.absa.africa", "web", 8080, owner("web"))},
			"local/apis/fleet.cattle.io/v1alpha1/clusters": {
				{"metadata": map[string]any{"name": "adonp270-cap-1", "namespace": "adonp270",
					"labels":      map[string]any{"management.cattle.io/cluster-name": "c-m-1"},
					"annotations": map[string]any{"clusterpool/legacy-cluster": "corp"}}},
			},
		},
	}
	servers := []*httptest.Server{httptest.NewServer(legacy), httptest.NewServer(target)}
	for _, s := range servers {
		defer s.Close()
	}
	t.Setenv("TOK", "tok")

	dir := t.TempDir()
	cfgJSON := `{"endpoints":[
	  {"name":"rke1","role":"legacy","url":"` + servers[0].URL + `","tokenEnv":"TOK"},
	  {"name":"rke2","role":"target","url":"` + servers[1].URL + `","tokenEnv":"TOK"}]}`
	cp := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cp, []byte(cfgJSON), 0o644)
	cfg, err := loadConfig(cp)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := runCollect(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep := buildSimpleReport(cfg, snap, "nonprod")
	if err := writeSimpleReports(rep, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	printSimpleSummary(os.Stdout, rep)
	if os.Getenv("SHOW") != "" {
		for _, f := range []string{"report.md", "rke1-inventory.csv", "found-in-rke2.csv", "missing-in-rke2.csv"} {
			c, _ := os.ReadFile(filepath.Join(dir, "out", f))
			fmt.Printf("\n--- %s ---\n%s", f, c)
		}
	}

	// 1. RKE1 inventory: both nonprod clusters present, prod and "weird" excluded.
	inv := readCSV(t, filepath.Join(dir, "out", "rke1-inventory.csv"))
	if len(inv) != 4 { // web(270), api(270), web(sdc), batch(sdc)
		t.Fatalf("inventory rows: %v", inv)
	}
	for _, r := range inv {
		if r["cluster"] == "cib-corp-prod-270" {
			t.Errorf("prod cluster must be excluded by env=nonprod: %v", r)
		}
	}
	if r := findRow(inv, "host", "api.np.absa.africa"); r["cluster"] != "cib-corp-nonprod-270" ||
		r["namespace"] != "shop" || r["gslb"] != "api-gslb" || r["ingress"] != "api-ing" {
		t.Errorf("api inventory row: %v", r)
	}

	// 2. Found in RKE2: web found on both RKE2 clusters, not flagged; api found but flagged
	// (different namespace on the RKE2 side).
	found := readCSV(t, filepath.Join(dir, "out", "found-in-rke2.csv"))
	if len(found) != 3 { // web(270), web(sdc), api(270)
		t.Fatalf("found rows: %v", found)
	}
	webRows := 0
	for _, r := range found {
		if r["host"] != "web.np.absa.africa" {
			continue
		}
		webRows++
		if r["found_on_rke2"] != "adonp270-cap-1;subnpsdc-cap-2" || r["flag"] != "" {
			t.Errorf("web found row: %v", r)
		}
	}
	if webRows != 2 {
		t.Errorf("expected a found row for web from each RKE1 cluster, got %d", webRows)
	}
	if r := findRow(found, "host", "api.np.absa.africa"); r["found_on_rke2"] != "adonp270-cap-1" || r["flag"] != "differs" {
		t.Errorf("api found row: %v", r)
	}

	// 3. Missing in RKE2: batch is nowhere on RKE2; its RKE1 cluster's base ("cib-corp")
	// resolves to adonp270-cap-1 via the legacy-cluster hint "corp".
	missing := readCSV(t, filepath.Join(dir, "out", "missing-in-rke2.csv"))
	if len(missing) != 1 {
		t.Fatalf("missing rows: %v", missing)
	}
	if m := missing[0]; m["host"] != "batch.np.absa.africa" || m["cluster"] != "cib-corp-nonprod-sdc" ||
		m["ingress"] != "batch" || m["likely_rke2_cluster"] != "adonp270-cap-1" {
		t.Errorf("missing batch row: %v", m)
	}
}

// TestCollectedAtSummary pins down the staleness warning that surfaces on every report,
// added after a real incident: report is offline/snapshot-based, so an annotation rollout
// completed in Rancher after the last collect silently didn't show up in likely_rke2_cluster,
// with nothing in the output hinting that the data might be old.
func TestCollectedAtSummary(t *testing.T) {
	fresh := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	if s := collectedAtSummary(fresh); strings.Contains(s, "STALE") {
		t.Errorf("a 5-minute-old snapshot must not be flagged stale: %s", s)
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if s := collectedAtSummary(old); !strings.Contains(s, "STALE") {
		t.Errorf("a 48-hour-old snapshot must be flagged stale: %s", s)
	}
	if s := collectedAtSummary(""); s != "unknown" {
		t.Errorf("empty CollectedAt should report unknown, got %q", s)
	}
}

// TestDesignatedTargets pins down pattern expansion against the exact syntax the real
// migration plan uses: "sub<EV><DC>-cap-N" (angle brackets, upper-case) in one table and
// "ssm{env}{dc}-cap-N" (curly braces, lower-case) in another -- both must resolve, since
// config normalisation lower-cases patterns on load either way.
func TestDesignatedTargets(t *testing.T) {
	cfg, err := loadConfig(writeTempConfig(t, `{"endpoints":[], "naming": {"clusterMap": {
		"cib-corp":       ["sub<EV><DC>-cap-4"],
		"cib-africatech": ["sub{env}{dc}-cap-2", "ado{env}{dc}-cap-1"],
		"cto-cloud":      ["ssm{env}{dc}-cap-0"],
		"subatomic":      ["fixed-literal-cluster"]
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		base, slot string
		want       []string
	}{
		{"cib-corp", "nonprod/270", []string{"subnp270-cap-4"}},
		{"cib-corp", "nonprod/sdc", []string{"subnpsdc-cap-4"}},
		{"cib-corp", "prod/sdc", []string{"subpdsdc-cap-4"}},
		{"cib-africatech", "nonprod/270", []string{"adonp270-cap-1", "subnp270-cap-2"}},
		{"cto-cloud", "nonprod/sdc", []string{"ssmnpsdc-cap-0"}},
		{"subatomic", "nonprod/270", []string{"fixed-literal-cluster"}}, // no placeholders -> unchanged
		{"no-such-base", "nonprod/270", nil},
	}
	for _, c := range cases {
		got := designatedTargets(cfg, c.base, c.slot)
		if !slicesEqual(got, c.want) {
			t.Errorf("designatedTargets(%q, %q) = %v, want %v", c.base, c.slot, got, c.want)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSimpleReportUsesClusterMapWithoutAnyAnnotation is the actual reported scenario: a
// migration plan (clusterMap) is configured, but Rancher's clusterpool/legacy-cluster
// annotation is completely absent (empty LegacyHint on every target cluster) -- either
// because it was never set or the snapshot predates it. likely_rke2_cluster and Designated
// must still resolve entirely from the plan; the annotation path must not be required.
func TestSimpleReportUsesClusterMapWithoutAnyAnnotation(t *testing.T) {
	legacy := &fakeRancher{
		clusters: []map[string]any{cl("c-1", "cib-corp-nonprod-270")},
		objs: map[string][]map[string]any{
			"c-1" + gp: {gslbEmbedded("shop", "web", "web.np.absa.africa", "roundRobin")},
			"c-1" + ip: {ing("shop", "web", "web.np.absa.africa", "web", 8080, owner("web"))},
		},
	}
	// No fleet-cluster annotation objects at all: the "local/apis/fleet.cattle.io/..." key
	// is simply absent, so fetchLegacyHints finds nothing and LegacyHint stays "" everywhere.
	target := &fakeRancher{
		clusters: []map[string]any{cl("c-m-1", "subnp270-cap-4")},
		objs: map[string][]map[string]any{
			"c-m-1" + gp: {},
			"c-m-1" + ip: {},
		},
	}
	servers := []*httptest.Server{httptest.NewServer(legacy), httptest.NewServer(target)}
	for _, s := range servers {
		defer s.Close()
	}
	t.Setenv("TOK", "tok")
	cfgJSON := `{"endpoints":[
	  {"name":"rke1","role":"legacy","url":"` + servers[0].URL + `","tokenEnv":"TOK"},
	  {"name":"rke2","role":"target","url":"` + servers[1].URL + `","tokenEnv":"TOK"}],
	  "naming": {"clusterMap": {"cib-corp": ["sub{env}{dc}-cap-4"]}}}`
	cfg, err := loadConfig(writeTempConfig(t, cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := runCollect(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range snap.Clusters {
		if c.Role == "target" && c.LegacyHint != "" {
			t.Fatalf("test setup: expected no legacy hint on the target cluster, got %q", c.LegacyHint)
		}
	}
	rep := buildSimpleReport(cfg, snap, "nonprod")
	if len(rep.Missing) != 1 {
		t.Fatalf("missing rows: %+v", rep.Missing)
	}
	if got := rep.Missing[0].LikelyRKE2; !slicesEqual(got, []string{"subnp270-cap-4"}) {
		t.Errorf("likely_rke2_cluster = %v, want [subnp270-cap-4] -- resolved from clusterMap with zero annotations present", got)
	}
}

func TestHintMatchesBase(t *testing.T) {
	cfg, err := loadConfig(writeTempConfig(t, `{"endpoints":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		hint, base string
		want       bool
	}{
		{"corp", "cib-corp", true},              // alias suffix match
		{"corp, fx", "cib-fx", true},             // comma-separated list
		{"cib-fx-nonprod-270", "cib-fx", true},   // full legacy name in the hint
		{"corp", "cib-fx", false},                // no match
		{"", "cib-corp", false},                  // empty hint
		{"absaaccess", "cib-absaaccess", true},
		{"amber", "cto-cloud", false}, // wholesale rename: unresolvable without legacyAliases
	}
	for _, c := range cases {
		if got := hintMatchesBase(cfg, c.hint, c.base); got != c.want {
			t.Errorf("hintMatchesBase(%q, %q) = %v, want %v", c.hint, c.base, got, c.want)
		}
	}
}

// TestHintMatchesBase_LegacyAliases covers renames with no textual relationship to the RKE1
// base name (e.g. "amber" for "cto-cloud") -- the suffix heuristic in TestHintMatchesBase
// can never resolve these; naming.legacyAliases is the explicit escape hatch for them.
func TestHintMatchesBase_LegacyAliases(t *testing.T) {
	cfg, err := loadConfig(writeTempConfig(t, `{"endpoints":[], "naming": {"legacyAliases": {
		"amber": "cto-cloud", "bolt": "avaf", "Cnc": "cto-shared"
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		hint, base string
		want       bool
	}{
		{"amber", "cto-cloud", true},
		{"AMBER", "cto-cloud", true}, // case-insensitive on both the hint and the configured key
		{"bolt", "avaf", true},
		{"cnc", "cto-shared", true}, // configured with a mixed-case key ("Cnc"), normalised on load
		{"amber", "avaf", false},    // must not cross-match another alias's base
		{"corp", "cib-corp", true},  // the suffix heuristic must still work alongside the map
	}
	for _, c := range cases {
		if got := hintMatchesBase(cfg, c.hint, c.base); got != c.want {
			t.Errorf("hintMatchesBase(%q, %q) = %v, want %v", c.hint, c.base, got, c.want)
		}
	}
}

// TestCLIBinaryEndToEnd builds the real binary and drives it as a subprocess through
// every command, via its actual Kong-parsed flags. Unlike the tests above -- which call
// buildSimpleReport/runCollect/etc. directly and so never touch main()'s flag parsing or
// dependency wiring -- this is what would have caught the "couldn't find binding of type
// context.Context" failure: Kong resolved fine, the *Config binding worked, but ctx (passed
// as a concrete value rather than via kong.BindFor[context.Context]) registered under its
// dynamic type instead of the context.Context interface, so no Run() method taking a
// context.Context could be matched. Exercise the CLI itself, not just the logic behind it.
func TestCLIBinaryEndToEnd(t *testing.T) {
	legacy := &fakeRancher{
		clusters: []map[string]any{cl("c-1", "cib-corp-nonprod-270")},
		objs: map[string][]map[string]any{
			"c-1" + gp: {gslbEmbedded("shop", "web", "web.np.absa.africa", "roundRobin")},
			"c-1" + ip: {ing("shop", "web", "web.np.absa.africa", "web", 8080, owner("web"))},
		},
	}
	target := &fakeRancher{
		clusters: []map[string]any{cl("c-m-1", "adonp270-cap-1")},
		objs: map[string][]map[string]any{
			"c-m-1" + gp: {gslbEmbedded("shop", "web", "web.np.absa.africa", "roundRobin")},
			"c-m-1" + ip: {ing("shop", "web", "web.np.absa.africa", "web", 8080, owner("web"))},
		},
	}
	s1, s2 := httptest.NewServer(legacy), httptest.NewServer(target)
	defer s1.Close()
	defer s2.Close()
	t.Setenv("RANCHER_RKE1_TOKEN", "tok")
	t.Setenv("RANCHER_MGMT1_TOKEN", "tok")

	dir := t.TempDir()
	cp := filepath.Join(dir, "config.json")
	cfgJSON := `{"endpoints":[
	  {"name":"rke1","role":"legacy","url":"` + s1.URL + `","tokenEnv":"RANCHER_RKE1_TOKEN"},
	  {"name":"rke2-mgmt-1","role":"target","url":"` + s2.URL + `","tokenEnv":"RANCHER_MGMT1_TOKEN"}]}`
	if err := os.WriteFile(cp, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "gslb-compare")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gslb-compare %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	if out := run("clusters", "--config", cp); !strings.Contains(out, "adonp270-cap-1") {
		t.Errorf("clusters output missing expected cluster:\n%s", out)
	}
	run("collect", "--config", cp, "--snapshot", "snap.json")
	if _, err := os.Stat(filepath.Join(dir, "snap.json")); err != nil {
		t.Errorf("collect should have written snap.json: %v", err)
	}
	run("report", "--config", cp, "--snapshot", "snap.json", "--out", "report", "--env", "nonprod")
	for _, f := range []string{"report.md", "rke1-inventory.csv", "found-in-rke2.csv", "missing-in-rke2.csv"} {
		if _, err := os.Stat(filepath.Join(dir, "report", f)); err != nil {
			t.Errorf("report should have written %s: %v", f, err)
		}
	}
	found := readCSV(t, filepath.Join(dir, "report", "found-in-rke2.csv"))
	if r := findRow(found, "host", "web.np.absa.africa"); r["found_on_rke2"] != "adonp270-cap-1" {
		t.Errorf("expected web.np.absa.africa found on adonp270-cap-1, got: %v", r)
	}

	run("run", "--config", cp, "--out", "report2", "--env", "nonprod")
	if _, err := os.Stat(filepath.Join(dir, "report2", "snapshot.json")); err != nil {
		t.Errorf("run should have written report2/snapshot.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report2", "report.md")); err != nil {
		t.Errorf("run should have written report2/report.md: %v", err)
	}

	// The short-flag form (-c) and the double-dash form must both work; a single-dash
	// long flag (-config) must not (that's Kong's parsing convention, worth pinning down
	// so a future flag-library swap doesn't silently change this again).
	run("clusters", "-c", cp)
	cmd := exec.Command(bin, "clusters", "-config", cp)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("expected -config (single-dash long flag) to be rejected, got:\n%s", out)
	}
}

func writeTempConfig(t *testing.T, json string) string {
	t.Helper()
	cp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cp, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	return cp
}

func readCSV(t *testing.T, path string) []map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, r := range recs[1:] {
		m := map[string]string{}
		for i, h := range recs[0] {
			m[h] = r[i]
		}
		out = append(out, m)
	}
	return out
}

func findRow(rows []map[string]string, key, val string) map[string]string {
	for _, r := range rows {
		if r[key] == val {
			return r
		}
	}
	return map[string]string{}
}

func TestTargetSlotPrecedenceAndAliasMerge(t *testing.T) {
	cp := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(cp, []byte(`{"endpoints":[
	  {"name":"rke1","role":"legacy","url":"http://x","tokenEnv":"TOK"},
	  {"name":"m1","role":"target","url":"http://x","tokenEnv":"TOK","slot":"Prod/SDC"},
	  {"name":"m2","role":"target","url":"http://x","tokenEnv":"TOK"}],
	  "naming":{"envAliases":{"pd":"prod"},"slotOverrides":{"odd":"nonprod/270"}}}`), 0o644)
	cfg, err := loadConfig(cp)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ ep, name, want string }{
		{"m1", "anything-goes", "prod/sdc"},     // endpoint slot, normalised
		{"m1", "adonp270-cap-1", "prod/sdc"},    // endpoint slot beats the name
		{"m1", "odd", "nonprod/270"},            // per-cluster override beats endpoint slot
		{"m2", "adonp270-cap-1", "nonprod/270"}, // regex
		{"m2", "anything-goes", ""},             // unknown -> wildcard
	}
	for _, c := range cases {
		if got := cfg.targetSlot(c.ep, c.name); got != c.want {
			t.Errorf("targetSlot(%s, %s) = %q, want %q", c.ep, c.name, got, c.want)
		}
	}
	// A partial envAliases in the config must not drop the built-in defaults.
	if _, slot, ok := cfg.parseLegacy("x-non-prod-270"); !ok || slot != "nonprod/270" {
		t.Errorf("alias merge: got %q ok=%v", slot, ok)
	}
	_ = os.WriteFile(cp, []byte(`{"endpoints":[{"name":"rke1","role":"legacy","url":"http://x","tokenEnv":"TOK","slot":"prod/sdc"}]}`), 0o644)
	if _, err := loadConfig(cp); err == nil {
		t.Error("slot on a legacy endpoint must be rejected")
	}
	_ = os.WriteFile(cp, []byte(`{"endpoints":[{"name":"m","role":"target","url":"http://x","tokenEnv":"TOK","slot":"prod"}]}`), 0o644)
	if _, err := loadConfig(cp); err == nil {
		t.Error("slot without env/dc must be rejected")
	}
}

func TestResourceRefHandling(t *testing.T) {
	toObj := func(m map[string]any, v any) {
		t.Helper()
		b, _ := json.Marshal(m)
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	var in k8sIngress
	toObj(ing("shop", "web", "web.np.absa.africa", "web", 8080, nil), &in)
	in.Metadata.Labels = map[string]string{"app": "web", "tier": "edge"}
	record := func(m map[string]any) GslbRecord {
		var g k8sGslb
		toObj(m, &g)
		return buildGslbRecord(g, []k8sIngress{in})
	}
	withRef := func(m map[string]any, ref map[string]any) map[string]any {
		m["spec"].(map[string]any)["resourceRef"] = ref
		return m
	}

	// "resourceRef": {} as the API returns it for a Gslb that doesn't use it -> embedded, linked by name.
	rec := record(withRef(gslbEmbedded("shop", "web", "web.np.absa.africa", "roundRobin"), map[string]any{}))
	if rec.Mode != "embedded" || len(rec.Hosts) != 1 || rec.Hosts[0].Ingress != "web" {
		t.Errorf("empty resourceRef: mode=%s hosts=%+v", rec.Mode, rec.Hosts)
	}
	// matchExpressions In
	rec = record(withRef(gslbRef("shop", "sel", "web.np.absa.africa", ""), map[string]any{"kind": "Ingress",
		"matchExpressions": []any{map[string]any{"key": "tier", "operator": "In", "values": []any{"edge"}}}}))
	if rec.Mode != "resourceRef" || len(rec.Hosts) != 1 || rec.Hosts[0].Ingress != "web" {
		t.Errorf("matchExpressions In: mode=%s hosts=%+v", rec.Mode, rec.Hosts)
	}
	// matchLabels + matchExpressions NotIn that excludes the ingress
	rec = record(withRef(gslbRef("shop", "sel", "web.np.absa.africa", ""), map[string]any{"kind": "Ingress",
		"matchLabels":      map[string]any{"app": "web"},
		"matchExpressions": []any{map[string]any{"key": "tier", "operator": "NotIn", "values": []any{"edge"}}}}))
	if len(rec.Hosts) != 1 || rec.Hosts[0].Ingress != "(not found)" {
		t.Errorf("matchExpressions NotIn must not link: %+v", rec.Hosts)
	}
	// DoesNotExist on a missing key links; Exists on a missing key doesn't
	rec = record(withRef(gslbRef("shop", "sel", "web.np.absa.africa", ""), map[string]any{"kind": "Ingress",
		"matchExpressions": []any{map[string]any{"key": "nope", "operator": "DoesNotExist"}}}))
	if rec.Hosts[0].Ingress != "web" {
		t.Errorf("DoesNotExist: %+v", rec.Hosts)
	}
	rec = record(withRef(gslbRef("shop", "sel", "web.np.absa.africa", ""), map[string]any{"kind": "Ingress",
		"matchExpressions": []any{map[string]any{"key": "nope", "operator": "Exists"}}}))
	if rec.Hosts[0].Ingress != "(not found)" {
		t.Errorf("Exists: %+v", rec.Hosts)
	}
}
