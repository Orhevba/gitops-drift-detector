# gitops-drift-detector

Compares Kubernetes manifests (your Git source of truth) against what's
actually running in a cluster, and reports drift:

- `MISSING` — declared in Git, not deployed
- `DRIFTED` — deployed, but fields differ from Git
- `ORPHAN`  — running in the cluster, not declared in Git
- `IN SYNC` — matches

Only compares fields that are present in the Git manifest — fields the
cluster/API server adds on its own (status, defaults, etc.) are ignored.

Kustomize is supported automatically: if the directory you point at
contains a `kustomization.yaml`, it's rendered with the Kustomize engine
first (see `examples/kustomize/overlays/dev`); otherwise every plain YAML
file in the directory is parsed as-is (see `examples/manifests`).

There are two ways to run it:

- **`cmd/check`** — a one-shot CLI. Point it at a local manifests directory
  and a kubeconfig, get a report, done.
- **`cmd/manager`** — an in-cluster controller. Define a `WatchedRepo`
  custom resource (Git repo URL + path + target namespace), and it
  continuously clones, checks, and writes the result to that resource's
  `.status`, on a schedule.

Both share the same comparison logic in `internal/drift`.

## Prerequisites

- Go 1.22+
- A kubeconfig pointing at your cluster (e.g. k3s: usually
  `/etc/rancher/k3s/k3s.yaml`, or copy it to `~/.kube/config`)

## CLI usage (`cmd/check`)

```sh
go mod tidy
go run ./cmd/check -manifests ./examples/manifests -namespace default
```

Point `-manifests` at your own repo of YAML files once you've verified it
works against the bundled example. Use `-kubeconfig` to target a specific
kubeconfig file if it's not in the default location or `$KUBECONFIG`.

Cluster-injected resources (e.g. `ConfigMap/kube-root-ca.crt`, which
Kubernetes adds to every namespace itself) are excluded from orphan
detection by default. Add your own exceptions with `-ignore`:

```sh
go run ./cmd/check -manifests ./examples/manifests -namespace default \
  -ignore "Secret/some-webhook-cert,ConfigMap/some-operator-cache"
```

Exit code is `1` if any drift was found, `0` if everything is in sync —
so it can be wired into CI.

## Running as a controller (`cmd/manager`)

This is the "real operator" version: instead of you running a command, it
runs inside the cluster and reconciles `WatchedRepo` resources on a loop.

1. **Install the CRD and RBAC:**
   ```sh
   kubectl apply -f config/crd/watchedrepo-crd.yaml
   kubectl apply -f config/rbac/rbac.yaml
   ```

2. **Build the image.** On a single-node k3s VM with no registry, the
   simplest path is building directly on that VM (copy the repo over, or
   `git clone` it there):
   ```sh
   docker build -t gitops-drift-detector:latest .
   ```
   If you're building elsewhere and need to get the image onto the k3s
   node, save and import it instead of pushing to a registry:
   ```sh
   docker save gitops-drift-detector:latest | ssh <vm> 'sudo k3s ctr images import -'
   ```

3. **Deploy the manager:**
   ```sh
   kubectl apply -f config/manager/deployment.yaml
   ```

4. **Create a `WatchedRepo`** (see `config/samples/watchedrepo-sample.yaml`
   for the shape) pointing at a real Git repo, then watch it get checked:
   ```sh
   kubectl apply -f config/samples/watchedrepo-sample.yaml
   kubectl get watchedrepos -A -w
   kubectl get watchedrepo demo -n drift-system -o yaml   # full status, including per-resource drift
   ```

RBAC note: the manager is granted broad cluster-wide read access (`get`/
`list`/`watch` on `*`/`*`), because a `WatchedRepo` can reference any
resource kind — the checker can't know ahead of time what to scope down
to. Fine for a personal cluster; tighten `config/rbac/rbac.yaml` to
specific apiGroups/resources before running this anywhere that matters.

## Drift notifications (Telegram)

The manager sends a Telegram message whenever a `WatchedRepo` **changes**
state (goes out of sync, or recovers) — not on every poll, so a
long-standing drift doesn't re-alert every `pollInterval` forever.

1. Message [@BotFather](https://t.me/BotFather) on Telegram, send `/newbot`,
   follow the prompts, and copy the bot token it gives you.
2. Send your new bot any message (in a DM, or add it to a group), then find
   your chat ID:
   ```sh
   curl -s "https://api.telegram.org/bot<TOKEN>/getUpdates" | grep -o '"chat":{"id":[0-9-]*'
   ```
3. Set both as environment variables. Locally (testing with `go run
   ./cmd/manager`):
   ```sh
   export TELEGRAM_BOT_TOKEN=<token>
   export TELEGRAM_CHAT_ID=<chat-id>
   go run ./cmd/manager
   ```
   In-cluster, create a Secret instead (see `config/manager/deployment.yaml`
   for the exact keys expected):
   ```sh
   kubectl create secret generic gitops-drift-detector-telegram \
     -n drift-system \
     --from-literal=token=<token> \
     --from-literal=chat-id=<chat-id>
   ```

If neither variable is set, notifications are silently disabled (a no-op)
— everything else still works.

## Auto-remediation (optional, off by default)

By default this tool only *reports* drift. Set `autoRemediate: true` on a
`WatchedRepo` and it will also *fix* it: missing resources are created,
and drifted ones are patched back to match Git — via [server-side
apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/),
so it only touches the fields it manages rather than overwriting fields
other tools or the cluster itself own.

```yaml
spec:
  autoRemediate: true   # create MISSING and fix DRIFTED resources
  pruneOrphans: true    # also delete resources not declared in Git (no effect without autoRemediate)
```

**Read this before turning it on:**
- `pruneOrphans: true` means the controller will `kubectl delete` anything
  in the target namespace of a kind your manifests use, that isn't in
  Git. If your manifests declare a `ConfigMap`, *every* `ConfigMap` in
  that namespace not in Git gets deleted, including ones you didn't
  realize were untracked. Start with `autoRemediate: true` and
  `pruneOrphans` left off, and only enable pruning once you're confident
  the namespace only contains what you expect.
- The manager's RBAC (`config/rbac/rbac.yaml`) grants
  `create`/`update`/`patch`/`delete` cluster-wide the moment it's
  installed at all — remediation isn't gated per-`WatchedRepo` at the
  RBAC layer, only in application logic. Anyone who can edit a
  `WatchedRepo` can turn on cluster-wide write access for the manager's
  identity.
- After remediating, the controller re-checks rather than assuming the
  patch worked (a patch can be accepted without producing the expected
  result, e.g. hitting an immutable field) — but that only catches
  *detectable* problems, not every possible side effect of an automated
  write to your cluster.

## Roadmap

- [x] Rebuild as a controller (`controller-runtime` + CRD)
- [x] Support Kustomize overlays, not just plain YAML
- [x] Alert on drift (Telegram; Slack/other webhooks can reuse the same `notify.Notifier` interface)
- [x] Optional auto-remediation (apply Git's version to fix drift automatically, via server-side apply)
