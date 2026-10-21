# rightsizing-check

Finds Kubernetes CPU and memory requests that are far above what the containers use, and says what
to set instead. By default it works **offline**: it reads your manifests and a usage file that you
exported, and never connects to a cluster. Optionally it reads the usage straight from a Prometheus
(`--prometheus`, with a `--window` such as `7d`), with plain read-only GET requests. It can also
read the workloads straight from a cluster (`--cluster`, list calls only), and it is a GitHub Action
that comments on the pull request.

The scheduler reserves what a pod *requests*, not what it uses. A request four times the real usage
keeps nodes half empty, and Karpenter cannot consolidate pods that say they need the capacity.
`rightsizing-check` lists the requests to lower, how much they free, and the workloads that are
under-requested, which are a risk of their own.

> **Status: early (v0.4.1).** It reads manifests or a live cluster, and four kinds of usage input. It is not a
> recommender that applies changes: it never edits your files. See [Limits](#limits).

## Why another one

[Goldilocks](https://github.com/FairwindsOps/goldilocks) and [KRR](https://github.com/robusta-dev/krr)
exist and are good. They run against a live cluster or a Prometheus. This one is for when you
cannot or do not want to: a CI job, a review of a client's cluster from files, an air-gapped
environment. You export the numbers once and the analysis is a pure function of your files. When
you do have a Prometheus, `--prometheus` fetches the two queries for you.

## Install

With Homebrew (macOS and Linux):

```bash
brew install robertasolimandonofreo/tap/rightsizing-check
```

Or download a binary for Linux, macOS or Windows (amd64 and arm64) from the
[releases page](https://github.com/robertasolimandonofreo/rightsizing-check/releases), and check it
against `checksums.txt`:

```bash
tar -xzf rightsizing-check_<version>_linux_amd64.tar.gz
sha256sum -c --ignore-missing checksums.txt
sudo mv rightsizing-check /usr/local/bin/
```

Or use the container image (linux/amd64 and linux/arm64), mounting your manifests and usage files:

```bash
docker run --rm -v "$PWD:/work:ro" ghcr.io/robertasolimandonofreo/rightsizing-check:latest \
  scan /work/k8s --csv /work/usage.csv
```

Or build it with Go:

```bash
go install github.com/robertasolimandonofreo/rightsizing-check/cmd/rightsizing-check@latest
```

## Use

```bash
rightsizing-check scan ./k8s --prometheus https://prometheus.example.com --window 7d
rightsizing-check scan --cluster --prometheus https://prometheus.example.com   # the cluster itself
rightsizing-check scan --cluster                                 # usage from metrics-server (an instant)
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

Pick one. The first two are the ones to trust.

**Straight from Prometheus.** One flag fetches both queries below and reads the result, over the
window you choose (it must include your busiest period; 7 days is a good start):

```bash
rightsizing-check scan ./k8s --prometheus https://prometheus.example.com --window 7d
rightsizing-check scan ./k8s --prometheus https://prometheus.example.com --window 1d \
  --prom-selector 'namespace=~"shop|nps"'          # a part of the cluster at a time
PROM_TOKEN=... rightsizing-check scan ./k8s --prometheus https://prometheus.example.com \
  --prom-token-env PROM_TOKEN                      # a bearer token, read from the environment
```

The address can be the one of the web UI (`/query` and `/graph` are removed). It only sends GET
requests to `/api/v1/query`, one query at a time, the memory one first. The window is any
Prometheus duration (`7d`, `168h`, `2w`).

**A long window is heavy.** The CPU query is a subquery over the whole window, so its cost grows
with the number of series times the window. On a small Prometheus a 14-day window made the pod use
enough memory to be evicted by the kubelet while this tool was being tested against a real server
(a 7-day window over about 190 thousand series took 45 seconds and was fine). So a window over 7
days is refused unless you pass `--prom-allow-long-window`. Start with `1d` or `7d`, read a few
namespaces at a time with `--prom-selector`, avoid peak hours, and never retry a query that answered
504 or 503: the server may still be running it. `--prom-timeout` (default 3 minutes) bounds each
query.

**By hand, from the same queries.** Two instant queries, over the same window. Export each result as
it comes:

```bash
PROM=http://prometheus:9090

# CPU: the 95th percentile of the per-container usage, in cores
curl -sG "$PROM/api/v1/query" --data-urlencode \
  'query=max by (namespace, pod, container) (quantile_over_time(0.95, rate(container_cpu_usage_seconds_total{container!="",container!="POD"}[5m])[7d:5m]))' > cpu.json

# Memory: the maximum working set, in bytes
curl -sG "$PROM/api/v1/query" --data-urlencode \
  'query=max by (namespace, pod, container) (max_over_time(container_memory_working_set_bytes{container!="",container!="POD"}[7d]))' > mem.json
```

Use `/api/v1/query`, not `query_range`: the tool wants one value per container. The same caution
about the window applies, and you then pass the files with `--prom-cpu` and `--prom-mem`.

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
| RC009 | caveat | An HPA or KEDA scaler on CPU or memory utilization would scale out if the request came down to the recommendation |
| RC010 | caveat | A VPA that is not in Off mode sets the requests of this workload, so a change by hand is overwritten |
| RC011 | data  | The VPA has its own recommendation (shown next to this tool's)                       |

`rightsizing-check explain RC001` documents any rule, with a problem and a fix.

## HPAs and KEDA

A HorizontalPodAutoscaler that scales on **utilization** compares the usage with the request, so
lowering the request raises the utilization it sees. When the scanned files include the HPA (or a
KEDA `ScaledObject` with a `cpu` or `memory` trigger), the tool computes what the scaler would see at
the recommended request and compares it with the target. RC009 fires only when it would pass the
target, with the numbers:

```
RC009 [caveat] ScaledObject api scales on cpu utilization (target 75%): it sees 8% of the request
today and would see 76% at the recommended request, so it would scale out, up to 6 replicas
```

Scalers that do not depend on the request are ignored: `AverageValue` targets, and every other KEDA
trigger (RabbitMQ, Prometheus, cron, HTTP). The summary adds how much of the reclaimable CPU sits on
such workloads. Scan the folder that holds your HPAs and ScaledObjects too, or export them from the
cluster (`kubectl get hpa -A -o yaml > hpa.yaml`) and pass the file.

## VPAs and Argo Rollouts

A VerticalPodAutoscaler that is not in `Off` mode (`Auto`, `Recreate`, `Initial` or
`InPlaceOrRecreate`; an unset mode is `Auto`) writes the requests of the pods it controls, so a
request changed by hand in the manifest does not last. When this tool would change a request that a
VPA controls, RC010 says so, and the summary adds how much of the reclaimable CPU sits on such
workloads: change those through the VPA (`minAllowed`, `maxAllowed`, or `Off` mode), not in the
manifest. A container that a resource policy takes out of the VPA (`mode: "Off"`) is not counted.

When the VPA object carries a status (a live cluster, or a manifest exported with
`kubectl get vpa -A -o yaml`), RC011 shows its target next to this tool's recommendation. The two can
differ because each learns from a different window.

An Argo `Rollout` with a pod template is analyzed like a Deployment, and its pods are matched the
same way. A Rollout that points at a Deployment (`workloadRef`) has no template of its own: the
Deployment is the workload that is analyzed.

## Live cluster

`--cluster` reads the workloads, the HPAs, the KEDA ScaledObjects, the VPAs and the Argo Rollouts
from the cluster instead of from files, and the usage of every container from the metrics API
(`metrics.k8s.io`, what `kubectl top` reads) unless you give another usage source.

```bash
rightsizing-check scan --cluster                                  # the current kubeconfig context
rightsizing-check scan --cluster --context prod -n shop           # one context, one namespace
rightsizing-check scan --cluster --prometheus https://prometheus.example.com --window 7d
```

It is **read-only**: only `list` calls are ever sent (the tests check that no other verb is used),
paged by 500 objects and limited to a few requests a second, one resource at a time. A custom
resource that is not installed is skipped with a note. The minimum RBAC:

```yaml
rules:
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "daemonsets"]
    verbs: ["list"]
  - apiGroups: ["autoscaling"]
    resources: ["horizontalpodautoscalers"]
    verbs: ["list"]
  - apiGroups: ["argoproj.io", "keda.sh", "autoscaling.k8s.io"]
    resources: ["rollouts", "scaledobjects", "verticalpodautoscalers"]
    verbs: ["list"]
  - apiGroups: ["metrics.k8s.io"]
    resources: ["pods"]
    verbs: ["list"]
```

Only Deployments, StatefulSets, DaemonSets and Rollouts are read: Jobs and CronJobs are not matched
to pods anyway. The usage from metrics-server is one instant and is flagged (RC008): with
`--prometheus` the metrics API is not read at all. Without `-n`, every namespace is read (`-n`
means the namespace of the `kubectl top` file only when `--cluster` is not used).

## GitHub Action

The repository is also a GitHub Action: it installs the release that matches its version (checking
the checksum), scans, adds the report to the job summary and, optionally, comments on the pull
request and fails the job.

```yaml
name: rightsizing
on: pull_request
permissions:
  contents: read
  pull-requests: write      # only needed when comment is true
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: robertasolimandonofreo/rightsizing-check@v0.4.1
        with:
          path: ./k8s
          prometheus: https://prometheus.example.com
          window: 7d
          prom-token: ${{ secrets.PROM_TOKEN }}   # optional
          comment: "true"
          fail-on: undersized
```

| Input | Default | |
| --- | --- | --- |
| `path` | `.` | Files or directories to scan, separated by spaces |
| `csv`, `prom-cpu`, `prom-mem`, `top`, `top-namespace` | empty | Usage from a file in the workspace |
| `prometheus`, `window`, `prom-selector`, `prom-token`, `allow-long-window` | empty, `7d` | Usage straight from a Prometheus |
| `fail-on` | empty | `undersized`, or `oversized` (which includes it). The comment is posted first |
| `comment` | `false` | Comment on the pull request, and keep that one comment updated |
| `slack`, `cpu-price`, `mem-price` | empty | The options of the same name |
| `version` | the action's version | A release such as `v0.4.0`; `local` uses the binary on the `PATH` |
| `github-token` | `github.token` | Token used to comment |

Outputs: `reclaimable-cpu-percent`, `reclaimable-memory-percent`, `oversized`, `undersized`, `ok` and
`no-data`. It runs on Linux and macOS runners. The comment is found by its marker, so it never
edits the comment of another tool. On pull requests from forks `GITHUB_TOKEN` is read-only, so
`comment` fails there. With `prometheus`, the usage is read **once** per run, and a window over 7d
is refused unless `allow-long-window` is `true`: see [the cost of a long window](#getting-the-usage-data).

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
- **HPA.** Read from the scanned manifests only (see [HPAs and KEDA](#hpas-and-keda)). An HPA that
  lives only in the cluster is not seen: export it and pass the file.
- **Memory is not compressible.** A request is a reservation, but a memory limit below the usage
  kills the container. Prefer the maximum over the window for memory, and do not shrink a container
  flagged RC005 before you know why it is close to its limit.
- **CPU limits are not analyzed**, and neither are init containers or sidecars injected at
  admission (they have no entry in the manifest).
- Pods are matched to workloads by name. A custom `generateName`, or a CRD that creates pods, is
  not recognized and shows up as unmatched.
- The Prometheus and `kubectl top` readers are tested against sample output, not against every
  version of them. If a file is rejected, `--csv` is the way out.

## Roadmap

Comparing with the base branch of a pull request, so that only a new finding fails the build, and
SARIF output, as spot-readiness does. Reading Jobs and CronJobs. None of it is started.

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

## More

- Guide and examples: https://www.robertasoliman.com/en/tools/rightsizing-check
- Article: https://www.robertasoliman.com/en/blog/rightsizing-check-find-inflated-kubernetes-requests

## License

Apache-2.0.
