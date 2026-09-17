# gslb-compare

Inventories k8gb `Gslb` objects (`k8gb.absa.oss/v1beta1`) and their related Ingresses on every
RKE1 downstream cluster behind one Rancher, and checks which of those hosts already exist on
the RKE2 downstream clusters behind any number of Rancher management servers, **matching on
hostname** (cluster names don't need to line up).

Talks to downstream clusters through the Rancher proxy (`/k8s/clusters/<id>`), so one Rancher
API token per management server is all you need. CLI flags via [kong](https://github.com/alecthomas/kong).

## Build

    go build -o gslb-compare .
    go test ./...          # end-to-end test against fake Rancher servers

## Run

    export RANCHER_RKE1_TOKEN='token-xxxxx:yyyy'   # one per endpoint, see tokenEnv
    export RANCHER_MGMT1_TOKEN=...                 # ... up to MGMT6
    cp config.example.json config.json             # set URLs

    ./gslb-compare clusters --config config.json    # 1. check name parsing, no downstream calls
    ./gslb-compare collect  --config config.json --snapshot snap.json   # 2. walk ~all clusters
    ./gslb-compare report   --config config.json --snapshot snap.json --out report   # 3. offline, repeatable

or `./gslb-compare run --config config.json --out report` for 2+3 in one go.

`--config`/`-c` is the only flag every command shares; the rest (`--snapshot`, `--out`,
`--env`, `--workers`) are per-command — run `./gslb-compare <command> --help` to see them.
**Flags are double-dash (`--out`) or short (`-c`); single-dash long flags (`-out`) are not
accepted** (that's Kong's parsing convention, not Go's stdlib `flag` package).

`--env` scopes *both* RKE1 and RKE2 to that env, both DCs (e.g. `nonprod` keeps
`nonprod/270` + `nonprod/sdc`, drops everything `prod`). Defaults to `nonprod`. Pass
`--env ""` for no filter at all.

`HTTPS_PROXY`/`NO_PROXY` are honoured; set `caFile` per endpoint for the Netskope CA.
The token needs read on clusters in `/v3`, read on `gslbs` + `ingresses` in each downstream
cluster, and (target endpoints, optional) read on `clusters.fleet.cattle.io` in `local`.

## How it works

**Collection** – per downstream cluster: list Gslbs (404 ⇒ k8gb not installed, skipped),
then Ingresses. Each Gslb is linked to its Ingress(es) by, in order: Gslb ownerRef → Ingress
(annotation mode), `spec.resourceRef` name/matchLabels, Ingress ownerRef → Gslb (embedded mode),
same name. Hosts come from the linked Ingress rules, `spec.ingress.rules` and `status.hosts`.

**Reporting** is three flat, independent lists — no app/group abstraction, no per-DC matrix.
A "cluster" is exactly one RKE1 or RKE2 cluster; env/dc are already part of its name
(`cib-corp-nonprod-270`), so listing by cluster is listing by DC for free.

| Section | What it is |
|---|---|
| **RKE1 inventory** | Every GSLB-fronted host, per RKE1 cluster (`cluster, namespace, gslb, host, ingress`). |
| **Found in RKE2** | Every RKE1 host from the inventory that also exists on *any* in-scope RKE2 cluster (regardless of pool), and which cluster(s). `designated` is where `naming.clusterMap` says this RKE1 cluster's apps belong. `flag` is `differs` when none of the RKE2 matches has the same namespace/GSLB/ingress name as RKE1, and/or `elsewhere` when it was found only off the designated cluster(s). |
| **Missing in RKE2** | Every RKE1 host not found on any in-scope RKE2 cluster. `likely_rke2_cluster` comes from `naming.clusterMap` (the migration plan) when the RKE1 cluster's base is in it; otherwise from a target cluster whose `clusterpool/legacy-cluster` annotation resolves back to that base — left blank when neither knows, never guessed. |

RKE1 clusters whose name doesn't match `naming.legacy`, or that fall outside `--env`, are
silently excluded from all three sections (check them with `clusters` first).
`hostRewrites` (`[{"from": ".old.zone", "to": ".new.zone"}]`) rewrites RKE1 host suffixes
before matching, for apps whose zone changed. `envAliases`/`dcAliases` in the config are
merged over the built-in defaults (`nonprod, non-prod, np, dev → nonprod; prod, prd, pd →
prod`), so a partial map is safe. `resourceRef` selectors support `matchLabels` and
`matchExpressions`; an empty `resourceRef: {}` (what the API returns for Gslbs that don't use
it) is ignored.

### Mapping RKE1 to RKE2

There are two independent ways `likely_rke2_cluster` / `designated` get filled in.
**`naming.clusterMap` is authoritative and should be your primary source** — it's the
migration plan itself, not an inference from Rancher metadata, so it doesn't depend on
whether an annotation has been applied yet or on any naming heuristic.

**`naming.clusterMap`** — `{"<RKE1 base name>": ["<RKE2 name pattern>", ...]}`. A pattern is
expanded against *each RKE1 cluster's own* env/dc: `{env}`/`{dc}` or `<env>`/`<dc>` (also
`{ev}`/`<ev>`) are substituted with the env token (`naming.envTokens`, default
`nonprod`→`np`, `prod`→`pd`) and the dc, so `"sub{env}{dc}-cap-4"` on `cib-corp-nonprod-270`
resolves to `subnp270-cap-4`, and on `cib-corp-prod-sdc` to `subpdsdc-cap-4`. A pattern with
no placeholders is used unchanged — only correct if that base has exactly one slot. A base
can map to more than one pattern (an app split across pools, e.g. `cib-africatech` landing on
both an `ado*` and a `sub*` cluster) — `report.md` lists every match under `Designated`.

`config.example.json`'s `clusterMap` is filled in from the actual migration plan for this
project, cross-checked against a full NS/NAME/LEGACY pull off live Rancher (every `ado`/`ssm`/
`sub` cluster, all four env/dc slots) — the legacy value is identical across all four slots for
a given `cap-N`, confirming one pattern per base is enough; no per-slot exceptions needed:

| Coverage | What's in there |
|---|---|
| `sub{env}{dc}-cap-0..20` and `ssm{env}{dc}-cap-0..3` | taken directly from the plan's "Confirmed" tables |
| `ado{env}{dc}-cap-0,1,2,3,5` (`cib-absaaccess`, `cib-africatech`, `cib-corp`, `cib-enablement`, `cib-markets`) | not in either plan table, but confirmed against the live NS/NAME/LEGACY pull, and consistent with the same alias used elsewhere in the plan |
| `fc-ftech` (`sub{env}{dc}-cap-16`), `pan-african-rtgs` (`ado{env}{dc}-cap-6`) | previously excluded for lack of a confirmed RKE1 base; the full live pull's `LEGACY` column gives these values verbatim (same pattern as `es-voice`, `rbb-banking` — a full literal name rather than a short alias), so they're now mapped directly, no `legacyAliases` entry needed |
| **deliberately overridden** — `cib-absaaccess`, `cib-africatech` | the plan's "for review" table proposed literal names (e.g. `cib-absaaccess-nonprod-sdc-cap-0`) that don't match what's actually live (`adonpsdc-cap-0`); the live name wins |

**`naming.legacyAliases`** — fallback only, used when a base has no `clusterMap` entry.
The `clusterpool/legacy-cluster` annotation is often a wholesale rename with no textual
relationship to the RKE1 cluster (`bolt` for `avaf`, `amber` for `cto-cloud`).
`likely_rke2_cluster` checks this map (`{"<annotation value>": "<RKE1 base name>"}`,
case-insensitive) before falling back further to a suffix heuristic that only covers
shortenings of the actual name (`corp` for `cib-corp`, `fx` for `cib-fx`). Note this whole
fallback only fires for bases *not* in `clusterMap`, and it depends on the annotation having
been collected — `clusterMap` doesn't.

## Output (`--out` dir)

| File | Content |
|---|---|
| `report.md` | all three sections as markdown tables, for reading |
| `rke1-inventory.csv` | section 1 |
| `found-in-rke2.csv` | section 2 |
| `missing-in-rke2.csv` | section 3 — the worklist |
| `snapshot.json` | raw collected data (`run` only) |
