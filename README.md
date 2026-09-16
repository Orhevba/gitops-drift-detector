# gitops-drift-detector

Compares a directory of Kubernetes manifests (your Git source of truth)
against what's actually running in a cluster, and reports drift:

- `MISSING` — declared in Git, not deployed
- `DRIFTED` — deployed, but fields differ from Git
- `ORPHAN`  — running in the cluster, not declared in Git
- `IN SYNC` — matches

Only compares fields that are present in the Git manifest — fields the
cluster/API server adds on its own (status, defaults, etc.) are ignored.

## Prerequisites

- Go 1.21+
- A kubeconfig pointing at your cluster (e.g. k3s: usually
  `/etc/rancher/k3s/k3s.yaml`, or copy it to `~/.kube/config`)

## Usage

```sh
go mod tidy
go run . -manifests ./examples/manifests -namespace default
```

Point `-manifests` at your own repo of YAML files once you've verified it
works against the bundled example. Use `-kubeconfig` to target a specific
kubeconfig file if it's not in the default location or `$KUBECONFIG`.

Cluster-injected resources (e.g. `ConfigMap/kube-root-ca.crt`, which
Kubernetes adds to every namespace itself) are excluded from orphan
detection by default. Add your own exceptions with `-ignore`:

```sh
go run . -manifests ./examples/manifests -namespace default \
  -ignore "Secret/some-webhook-cert,ConfigMap/some-operator-cache"
```

Exit code is `1` if any drift was found, `0` if everything is in sync —
so it can be wired into CI later.

## Roadmap

- [ ] Support Kustomize overlays, not just plain YAML
- [ ] Run on a schedule (K8s CronJob) instead of manually
- [ ] Slack/webhook alert on drift
- [ ] Rebuild as a proper controller (`controller-runtime` + CRD) with
      optional auto-remediation
