package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const gslbPath = "/apis/k8gb.absa.oss/v1beta1/gslbs"
const ingressPath = "/apis/networking.k8s.io/v1/ingresses"
const fleetClustersPath = "/apis/fleet.cattle.io/v1alpha1/clusters"

type collectJob struct {
	rc  *rancherClient
	idx int
}

// discover lists clusters on every endpoint and returns the skeleton snapshot plus jobs.
func discover(ctx context.Context, cfg *Config) (*Snapshot, []collectJob, error) {
	snap := &Snapshot{CollectedAt: time.Now().UTC().Format(time.RFC3339)}
	type pending struct {
		rc *rancherClient
		cd ClusterData
	}
	var all []pending

	for _, ep := range cfg.Endpoints {
		rc, err := newRancherClient(ep, time.Duration(cfg.TimeoutSeconds)*time.Second)
		if err != nil {
			return nil, nil, err
		}
		var inc, exc *regexp.Regexp
		if ep.ClusterInclude != "" {
			inc = regexp.MustCompile(ep.ClusterInclude)
		}
		if ep.ClusterExclude != "" {
			exc = regexp.MustCompile(ep.ClusterExclude)
		}
		clusters, err := rc.listClusters(ctx)
		if err != nil {
			log.Printf("WARN %s: cannot list clusters: %v", ep.Name, err)
			all = append(all, pending{cd: ClusterData{Endpoint: ep.Name, Role: ep.Role, Name: "<endpoint " + ep.Name + ">", Error: err.Error()}})
			continue
		}
		hints := map[string]string{}
		if ep.Role == "target" {
			hints = fetchLegacyHints(ctx, rc, cfg.LegacyAnnotation)
		}
		log.Printf("%s (%s): %d clusters, %d fleet annotation hints", ep.Name, ep.Role, len(clusters), countPrefix(hints, "name:"))
		for _, cl := range clusters {
			if cl.ID == "local" && !ep.IncludeLocal {
				continue
			}
			if (inc != nil && !inc.MatchString(cl.Name)) || (exc != nil && exc.MatchString(cl.Name)) {
				continue
			}
			cd := ClusterData{Endpoint: ep.Name, Role: ep.Role, ID: cl.ID, Name: cl.Name, State: cl.State, Labels: cl.Labels}
			if h, ok := hints["id:"+cl.ID]; ok {
				cd.LegacyHint = h
			} else if h, ok := hints["name:"+cl.Name]; ok {
				cd.LegacyHint = h
			}
			if cl.State != "active" && !cfg.AllStates {
				cd.Skipped = "state=" + cl.State
			}
			all = append(all, pending{rc: rc, cd: cd})
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].cd.Endpoint != all[j].cd.Endpoint {
			return all[i].cd.Endpoint < all[j].cd.Endpoint
		}
		return all[i].cd.Name < all[j].cd.Name
	})
	var jobs []collectJob
	for i, p := range all {
		snap.Clusters = append(snap.Clusters, p.cd)
		if p.rc != nil && p.cd.Skipped == "" && p.cd.Error == "" {
			jobs = append(jobs, collectJob{rc: p.rc, idx: i})
		}
	}
	return snap, jobs, nil
}

// fetchLegacyHints reads clusters.fleet.cattle.io on the management ("local") cluster
// and returns annotation values keyed by "id:<c-m-xxx>" and "name:<display name>".
func fetchLegacyHints(ctx context.Context, rc *rancherClient, annotation string) map[string]string {
	out := map[string]string{}
	fcs, err := listAll[fleetCluster](ctx, rc, proxyPath("local")+fleetClustersPath)
	if err != nil {
		log.Printf("WARN %s: cannot read fleet clusters for %s hints: %v", rc.ep.Name, annotation, err)
		return out
	}
	for _, fc := range fcs {
		v := strings.TrimSpace(fc.Metadata.Annotations[annotation])
		if v == "" {
			continue
		}
		l := fc.Metadata.Labels
		if id := l["management.cattle.io/cluster-name"]; id != "" {
			out["id:"+id] = v
		}
		name := l["management.cattle.io/cluster-display-name"]
		if name == "" {
			name = fc.Metadata.Name
		}
		out["name:"+name] = v
	}
	return out
}

func runCollect(ctx context.Context, cfg *Config) (*Snapshot, error) {
	snap, jobs, err := discover(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var done int64
	total := len(jobs)
	ch := make(chan collectJob)
	var wg sync.WaitGroup
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				cd := &snap.Clusters[j.idx]
				cctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
				err := collectCluster(cctx, j.rc, cd)
				cancel()
				n := atomic.AddInt64(&done, 1)
				switch {
				case err != nil:
					cd.Error = err.Error()
					log.Printf("[%d/%d] %s/%s ERROR %v", n, total, cd.Endpoint, cd.Name, err)
				case !cd.HasK8gb:
					log.Printf("[%d/%d] %s/%s no k8gb CRD", n, total, cd.Endpoint, cd.Name)
				default:
					log.Printf("[%d/%d] %s/%s %d gslb", n, total, cd.Endpoint, cd.Name, len(cd.Gslbs))
				}
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return snap, nil
}

func collectCluster(ctx context.Context, rc *rancherClient, cd *ClusterData) error {
	base := proxyPath(cd.ID)
	gslbs, err := listAll[k8sGslb](ctx, rc, base+gslbPath)
	if isStatus(err, 404) {
		return nil // CRD not installed
	}
	if err != nil {
		return fmt.Errorf("list gslbs: %w", err)
	}
	cd.HasK8gb = true
	if len(gslbs) == 0 {
		return nil
	}
	ings, err := listAll[k8sIngress](ctx, rc, base+ingressPath)
	if err != nil {
		return fmt.Errorf("list ingresses: %w", err)
	}
	byNS := map[string][]k8sIngress{}
	for _, in := range ings {
		byNS[in.Metadata.Namespace] = append(byNS[in.Metadata.Namespace], in)
	}
	for _, g := range gslbs {
		cd.Gslbs = append(cd.Gslbs, buildGslbRecord(g, byNS[g.Metadata.Namespace]))
	}
	sort.Slice(cd.Gslbs, func(i, j int) bool {
		if cd.Gslbs[i].Namespace != cd.Gslbs[j].Namespace {
			return cd.Gslbs[i].Namespace < cd.Gslbs[j].Namespace
		}
		return cd.Gslbs[i].Name < cd.Gslbs[j].Name
	})
	return nil
}

func buildGslbRecord(g k8sGslb, nsIngs []k8sIngress) GslbRecord {
	s := g.Spec.Strategy
	rec := GslbRecord{
		Namespace: g.Metadata.Namespace,
		Name:      g.Metadata.Name,
		GeoTag:    g.Status.GeoTag,
		Strategy: Strategy{
			Type: s.Type, PrimaryGeoTag: s.PrimaryGeoTag,
			DNSTtlSeconds: s.DNSTtlSeconds, SplitBrainThresholdSeconds: s.SplitBrainThresholdSeconds,
		},
	}
	if len(s.Weight) > 0 {
		rec.Strategy.Weight = map[string]string{}
		for k, v := range s.Weight {
			rec.Strategy.Weight[k] = fmt.Sprint(v)
		}
	}

	ingOwner := map[string]bool{}
	for _, o := range g.Metadata.OwnerReferences {
		if o.Kind == "Ingress" {
			ingOwner[o.Name] = true
		}
	}
	// The API returns "resourceRef": {} for Gslbs that don't use it (non-pointer struct in
	// k8gb), so only treat it as set when it actually references something.
	rr := g.Spec.ResourceRef
	if rr != nil && rr.Kind == "" && rr.Name == "" && len(rr.MatchLabels) == 0 && len(rr.MatchExpressions) == 0 {
		rr = nil
	}
	switch {
	case rr != nil:
		rec.Mode = "resourceRef"
	case len(ingOwner) > 0:
		rec.Mode = "ingress-annotation"
	default:
		rec.Mode = "embedded"
	}

	// Link the related Ingress objects.
	var linked []k8sIngress
	for _, in := range nsIngs {
		m := in.Metadata
		ok := ingOwner[m.Name] || ownedBy(m, "Gslb", g.Metadata.Name)
		if rr != nil && rr.Kind == "Ingress" {
			ok = ok || (rr.Name != "" && rr.Name == m.Name) ||
				(rr.Name == "" && (len(rr.MatchLabels) > 0 || len(rr.MatchExpressions) > 0) &&
					labelsMatch(rr.MatchLabels, m.Labels) && exprsMatch(rr.MatchExpressions, m.Labels))
		}
		if rr == nil {
			ok = ok || m.Name == g.Metadata.Name
		}
		if ok {
			linked = append(linked, in)
		}
	}

	// Collect hosts in a stable order.
	var hosts []string
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	for _, in := range linked {
		for _, r := range in.Spec.Rules {
			add(r.Host)
		}
	}
	if g.Spec.Ingress != nil {
		for _, r := range g.Spec.Ingress.Rules {
			add(r.Host)
		}
	}
	for _, h := range strings.Split(g.Status.Hosts, ",") {
		add(h)
	}
	sort.Strings(hosts)

	for _, h := range hosts {
		hr := HostRecord{Host: h, Health: g.Status.ServiceHealth[h]}
		if recs := g.Status.HealthyRecords[h]; len(recs) > 0 {
			hr.Records = append([]string(nil), recs...)
			sort.Strings(hr.Records)
		}
		var spec *ingressSpec
		var ann map[string]string
		for i := range linked {
			if specHasHost(&linked[i].Spec, h) {
				spec, ann = &linked[i].Spec, linked[i].Metadata.Annotations
				hr.Ingress = linked[i].Metadata.Name
				break
			}
		}
		if spec == nil && g.Spec.Ingress != nil && specHasHost(g.Spec.Ingress, h) {
			spec = g.Spec.Ingress
			hr.Ingress = "(embedded; Ingress object not found)"
		}
		if spec == nil {
			switch {
			case rr != nil && rr.Kind != "Ingress":
				hr.Ingress = fmt.Sprintf("%s/%s (not inspected)", rr.Kind, rr.Name)
			default:
				hr.Ingress = "(not found)"
			}
		} else {
			hr.IngressClass, hr.TLSSecret, hr.Paths = describeHost(spec, ann, h)
		}
		rec.Hosts = append(rec.Hosts, hr)
	}
	return rec
}

func ownedBy(m objectMeta, kind, name string) bool {
	for _, o := range m.OwnerReferences {
		if o.Kind == kind && o.Name == name {
			return true
		}
	}
	return false
}

func labelsMatch(sel, labels map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// exprsMatch evaluates LabelSelector matchExpressions the way the API server does.
func exprsMatch(exprs []labelSelectorReq, labels map[string]string) bool {
	for _, e := range exprs {
		v, has := labels[e.Key]
		switch e.Operator {
		case "In":
			if !has || !slices.Contains(e.Values, v) {
				return false
			}
		case "NotIn":
			if has && slices.Contains(e.Values, v) {
				return false
			}
		case "Exists":
			if !has {
				return false
			}
		case "DoesNotExist":
			if has {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func specHasHost(s *ingressSpec, h string) bool {
	for _, r := range s.Rules {
		if strings.EqualFold(r.Host, h) {
			return true
		}
	}
	return false
}

func hostMatches(pattern, h string) bool {
	pattern = strings.ToLower(pattern)
	if pattern == h {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		rest := strings.TrimSuffix(h, suffix)
		return rest != h && rest != "" && !strings.Contains(rest, ".")
	}
	return false
}

func fmtBackend(b ingressBackend) string {
	switch {
	case b.Service != nil:
		port := b.Service.Port.Name
		if port == "" {
			port = fmt.Sprint(b.Service.Port.Number)
		}
		return b.Service.Name + ":" + port
	case b.Resource != nil:
		return b.Resource.Kind + "/" + b.Resource.Name
	}
	return "?"
}

func describeHost(s *ingressSpec, ann map[string]string, h string) (class, tlsSecret string, paths []string) {
	if s.IngressClassName != nil {
		class = *s.IngressClassName
	} else if v := ann["kubernetes.io/ingress.class"]; v != "" {
		class = v
	}
	var secrets []string
	for _, t := range s.TLS {
		for _, th := range t.Hosts {
			if hostMatches(th, h) {
				secrets = append(secrets, t.SecretName)
				break
			}
		}
	}
	sort.Strings(secrets)
	tlsSecret = strings.Join(secrets, ",")
	for _, r := range s.Rules {
		if !strings.EqualFold(r.Host, h) {
			continue
		}
		if r.HTTP == nil {
			if s.DefaultBackend != nil {
				paths = append(paths, "(default) -> "+fmtBackend(*s.DefaultBackend))
			}
			continue
		}
		for _, p := range r.HTTP.Paths {
			pt := ""
			if p.PathType != nil {
				pt = *p.PathType
			}
			path := p.Path
			if path == "" {
				path = "/"
			}
			paths = append(paths, fmt.Sprintf("%s[%s] -> %s", path, pt, fmtBackend(p.Backend)))
		}
	}
	sort.Strings(paths)
	return
}

func countPrefix(m map[string]string, prefix string) int {
	n := 0
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}
