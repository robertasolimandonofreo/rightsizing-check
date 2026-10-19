# rightsizing-check

Finds Kubernetes CPU and memory requests that are far above what the containers use, and says what
to set instead. It works **offline**: it reads your manifests and a usage file that you exported,
and never connects to a cluster or a metrics server.

The scheduler reserves what a pod *requests*, not what it uses. A request four times the real usage
keeps nodes half empty, and Karpenter cannot consolidate pods that say they need the capacity.
`rightsizing-check` lists the requests to lower, how much they free, and the workloads that are
under-requested, which are a risk of their own.

> **Status: early (v0.1).** It reads manifests and three kinds of usage file. It is not a
> recommender that applies changes: it never edits your files. See [Limits](#limits).

## Why another one

[Goldilocks](https://github.com/FairwindsOps/goldilocks) and [KRR](https://github.com/robusta-dev/krr)
exist and are good. They run against a live cluster or a Prometheus. This one is for when you
cannot or do not want to: a CI job, a review of a client's cluster from files, an air-gapped
environment. You export the numbers once and the analysis is a pure function of your files.

## Install

Build it with Go (a release with binaries and a container image comes with the first tag):

```bash
go install github.com/robertasolimandonofreo/rightsizing-check/cmd/rightsizing-check@latest
```

## Use

```bash
rightsizing-check scan ./k8s --csv usage.csv
rightsizing-check scan ./k8s --prom-cpu cpu.json --prom-mem mem.json
rightsizing-check scan ./k8s --top top.txt -n shop
helm template my-release ./chart | rightsizing-check scan - --prom-cpu cpu.json --prom-mem mem.json
rightsizing-check scan ./k8s --csv usage.csv -v                  # every finding
rightsizing-check scan ./k8s --csv usage.csv -f json             # machine-readable
rightsizing-check scan ./k8s --csv usage.csv -f markdown         # for a pull request comment
rightsizing-check scan ./k8s --csv usage.csv --cpu-price 25 --mem-price 4   # a cost line
rightsizing-check scan ./k8s --csv usage.csv --fail-on undersized           # exit 1 in CI
rightsizing-check explain RC001                                  # what a rule means
```

Example output, from `testdata/` (`scan testdata/sample.yaml --csv testdata/usage.csv`):

```
STATUS      WORKLOAD                     CONTAINER  REPLICAS  CPU REQ  CPU USE  CPU REC  MEM REQ  MEM USE  MEM REC  FINDINGS
oversized   shop/deployment/api          app        3         1000m    120m     140m     2Gi      600Mi    720Mi    RC001,RC002
oversized   kube-system/daemonset/agent  agent      -         500m     40m      50m      512Mi    120Mi    144Mi    RC001,RC002
undersized  shop/deployment/worker       worker     2         100m     320m     370m     256Mi    576Mi    692Mi    RC003,RC004,RC005
undersized  shop/statefulset/db          db         1         -        450m     520m     -        1.5Gi    1844Mi   RC006
ok          shop/deployment/web          web        2         150m     120m     140m     200Mi    144Mi    176Mi    -
no-data     shop/cronjob/report          report     -         -        ?        ?        -        ?        ?        RC007
```

## Getting the usage data

Pick one. The first is the one to trust.

**Prometheus (recommended).** Two instant queries, over a window that includes your busiest period
(7 days is a good start). Export each result as it comes:

```bash
PROM=http://prometheus:9090

# CPU: the 95th percentile of the per-container usage, in cores
curl -sG "$PROM/api/v1/query" --data-urlencode \
  'query=max by (namespace, pod, container) (quantile_over_time(0.95, rate(container_cpu_usage_seconds_total{container!="",container!="POD"}[5m])[7d:5m]))' > cpu.json

# Memory: the maximum working set, in bytes
curl -sG "$PROM/api/v1/query" --data-urlencode \
  'query=max by (namespace, pod, container) (max_over_time(container_memory_working_set_bytes{container!="",container!="POD"}[7d]))' > mem.json
```

Use `/api/v1/query`, not `query_range`: the tool wants one value per container. The CPU query is a
subquery and can be heavy on a large Prometheus: shorten the window, or query per namespace.

**CSV.** From Grafana, a notebook or anywhere else. The header is required, and a blank value means
that number is not known:

```
namespace,pod,container,cpu_millis,memory_bytes
shop,api-5d8f7c9b6-x2k4j,app,120,629145600
```

**`kubectl top`.** Quick, and the weakest: one instant, not a percentile. The tool flags it (RC008).

```bash
kubectl top pods -A --containers > top.txt
```

## How pods are matched to workloads

Usage is per pod, requests are per workload, so pods are matched by name in the same namespace, the
way the controllers name them: `name-<hash>-<suffix>` for a Deployment, `name-<ordinal>` for a
StatefulSet, `name-<suffix>` for a DaemonSet, and the exact name for a bare Pod. The random parts use
the alphabet Kubernetes uses (no vowels), so `api-gateway-extra` is never taken for a pod of `api`.
Pods that match nothing are counted in the summary. Jobs and CronJobs are not matched: their pods
are short-lived.

With several replicas, **the busiest one decides**: one hot pod needs the request even if its
siblings idle.

## The recommendation

| Resource | Recommended request                                            |
| -------- | -------------------------------------------------------------- |
| CPU      | observed usage plus 15% headroom, rounded up to 10m (minimum 10m)   |
| Memory   | observed usage plus 20% headroom, rounded up to 4Mi (minimum 16Mi)  |

A request is **inflated** when it is above 1.5 times the recommendation (`--slack`), which means a
utilization under about 58%. The headrooms are `--cpu-headroom` and `--mem-headroom`.

## Statuses

| Status       | Meaning                                                                          |
| ------------ | -------------------------------------------------------------------------------- |
| `oversized`  | A request is far above the usage, and nothing is at risk                          |
| `undersized` | A request or limit is below the usage, or a request is missing. It outranks oversized |
| `ok`         | The requests are close to the usage                                              |
| `no-data`    | No container of the workload has a usage sample                                  |

## Rules

| Rule  | Kind  | What it checks                                                       |
| ----- | ----- | -------------------------------------------------------------------- |
| RC001 | waste | CPU request far above the observed usage                             |
| RC002 | waste | Memory request far above the observed usage                          |
| RC003 | risk  | CPU usage above the request                                          |
| RC004 | risk  | Memory usage above the request                                       |
| RC005 | risk  | Memory usage within 10% of the limit (an OOMKill is one spike away)  |
| RC006 | data  | No request is set                                                    |
| RC007 | data  | No usage found for the container                                     |
| RC008 | data  | Usage is a `kubectl top` snapshot, not a window                      |

`rightsizing-check explain RC001` documents any rule, with a problem and a fix.

## What the numbers mean

*Reclaimable requests* is what the recommendations free: for each inflated request, the request
minus the recommendation, times the replicas. It covers Deployments, StatefulSets and Pods. A
DaemonSet has no replica count (it depends on the nodes) and is listed per pod and left out of the
sums. The percentages are over the requests of the workloads that have usage data.

The cost line (`--cpu-price`, `--mem-price`, per vCPU-month and per GiB-month) is your price times
the reclaimable amount. The tool has no prices of its own. It is a figure in requests, not in your
bill: it becomes savings only when the nodes consolidate, which is what Karpenter does once the
pods stop asking for the capacity.

## Exit codes

`0` success, `1` the `--fail-on` level was reached (`undersized`, or `oversized`, which includes
undersized), `2` a usage or runtime error.

## JSON

The JSON starts with `schemaVersion` and `toolVersion`, then the `config` (headrooms and slack), a
`summary` and one entry per workload with its status, its containers (request, limit, usage,
recommendation, reclaimable) and its findings.

## Limits

- **A recommendation is a starting point.** It is only as good as the window of your data. A job that
  runs once a month, or a spike you did not capture, will make a request look inflated. Use a
  window that covers your busiest period, and lower requests one workload at a time.
- **HPA.** A HorizontalPodAutoscaler that scales on CPU utilization uses the request as the
  denominator. Lowering the request raises the utilization the HPA sees, and it can scale out.
  The tool does not read HPAs yet: check them before you lower a CPU request.
- **Memory is not compressible.** A request is a reservation, but a memory limit below the usage
  kills the container. Prefer the maximum over the window for memory, and do not shrink a container
  flagged RC005 before you know why it is close to its limit.
- **CPU limits are not analyzed**, and neither are VPAs, init containers or sidecars injected at
  admission (they have no entry in the manifest).
- Pods are matched to workloads by name. A custom `generateName`, or a CRD that creates pods, is
  not recognized and shows up as unmatched.
- The Prometheus and `kubectl top` readers are tested against sample output, not against every
  version of them. If a file is rejected, `--csv` is the way out.

## Roadmap

Reading HPAs and VPAs, Argo Rollouts and KEDA, a live-cluster mode (read-only), and a GitHub Action
with a pull request comment. None of it is started.

## Related

[spot-readiness](https://github.com/robertasolimandonofreo/spot-readiness) checks which workloads
tolerate Spot, and
[graviton-readiness](https://github.com/robertasolimandonofreo/graviton-readiness) which can run on
arm64. Lowering inflated requests first makes both moves cheaper.

## Development

```bash
go test -race ./...
go run ./cmd/rightsizing-check scan testdata/sample.yaml --csv testdata/usage.csv
```

## License

Apache-2.0.
