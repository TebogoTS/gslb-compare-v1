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
| **Found in RKE2** | Every RKE1 host from the inventory that also exists on *any* in-scope RKE2 cluster (regardless of pool), and which cluster(s). `flag=differs` when none of the RKE2 matches has the same namespace/GSLB/ingress name as the RKE1 side. |
| **Missing in RKE2** | Every RKE1 host not found on any in-scope RKE2 cluster. `likely_rke2_cluster` is filled in only when a target cluster's `clusterpool/legacy-cluster` Rancher annotation already resolves back to the RKE1 cluster's base name (tolerates a dropped prefix/suffix) — left blank otherwise, never guessed. |

RKE1 clusters whose name doesn't match `naming.legacy`, or that fall outside `--env`, are
silently excluded from all three sections (check them with `clusters` first).
`hostRewrites` (`[{"from": ".old.zone", "to": ".new.zone"}]`) rewrites RKE1 host suffixes
before matching, for apps whose zone changed. `envAliases`/`dcAliases` in the config are
merged over the built-in defaults (`nonprod, non-prod, np, dev → nonprod; prod, prd, pd →
prod`), so a partial map is safe. `resourceRef` selectors support `matchLabels` and
`matchExpressions`; an empty `resourceRef: {}` (what the API returns for Gslbs that don't use
it) is ignored.

**`naming.legacyAliases`** — the `clusterpool/legacy-cluster` annotation is often a wholesale
rename with no textual relationship to the RKE1 cluster (`bolt` for `avaf`, `amber` for
`cto-cloud`). `likely_rke2_cluster` first checks this map (`{"<annotation value>": "<RKE1 base
name>"}`, case-insensitive), then falls back to a suffix heuristic that only covers
shortenings of the actual name (`corp` for `cib-corp`, `fx` for `cib-fx`). Aliases with no
entry here and no textual relationship to their RKE1 base will never resolve automatically —
that's expected, not a bug; add them here once you know them.

## Output (`--out` dir)

| File | Content |
|---|---|
| `report.md` | all three sections as markdown tables, for reading |
| `rke1-inventory.csv` | section 1 |
| `found-in-rke2.csv` | section 2 |
| `missing-in-rke2.csv` | section 3 — the worklist |
| `snapshot.json` | raw collected data (`run` only) |
