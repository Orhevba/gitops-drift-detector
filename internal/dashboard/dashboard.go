// Package dashboard serves a small read-only HTML page listing every
// WatchedRepo the manager knows about and its current sync status.
package dashboard

import (
	"html/template"
	"net/http"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	driftv1alpha1 "github.com/tygacookie/gitops-drift-detector/api/v1alpha1"
)

// NewHandler returns an http.Handler that lists WatchedRepo resources via c.
func NewHandler(c client.Client) http.Handler {
	return &handler{client: c}
}

type handler struct {
	client client.Client
}

func (h *handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var list driftv1alpha1.WatchedRepoList
	if err := h.client.List(req.Context(), &list); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sort.Slice(list.Items, func(i, j int) bool {
		if list.Items[i].Namespace != list.Items[j].Namespace {
			return list.Items[i].Namespace < list.Items[j].Namespace
		}
		return list.Items[i].Name < list.Items[j].Name
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, list.Items); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

const pageHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>gitops-drift-detector</title>
<meta http-equiv="refresh" content="30">
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; background: #0f172a; color: #e2e8f0; }
  h1 { font-weight: 600; font-size: 1.25rem; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 0.5rem 0.75rem; border-bottom: 1px solid #334155; vertical-align: top; }
  th { color: #94a3b8; font-weight: 500; font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.03em; }
  .in-sync { color: #4ade80; font-weight: 600; }
  .drifted { color: #f87171; font-weight: 600; }
  .error-badge { color: #fbbf24; font-weight: 600; }
  .drift-list { margin: 0; padding-left: 1.1rem; color: #cbd5e1; font-size: 0.85rem; }
  .empty { color: #64748b; padding: 2rem 0; }
  .target { color: #94a3b8; font-size: 0.85rem; }
  .remediate-badge { font-size: 0.7rem; color: #38bdf8; border: 1px solid #38bdf8; border-radius: 3px; padding: 0 4px; margin-left: 4px; }
</style>
</head>
<body>
<h1>WatchedRepo status</h1>
{{if not .}}<p class="empty">No WatchedRepo resources found.</p>{{end}}
{{if .}}
<table>
<tr><th>Namespace</th><th>Name</th><th>Target</th><th>Status</th><th>Detail</th><th>Last Checked</th></tr>
{{range .}}
<tr>
  <td>{{.Namespace}}</td>
  <td>{{.Name}}{{if .Spec.AutoRemediate}} <span class="remediate-badge">auto</span>{{end}}</td>
  <td class="target">{{.Spec.RepoURL}}@{{.Spec.Branch}}<br>{{.Spec.Path}} &rarr; ns/{{.Spec.Namespace}}{{if .Spec.KubeconfigSecretRef}} (remote cluster){{end}}</td>
  <td>
    {{if .Status.Error}}<span class="error-badge">ERROR</span>
    {{else if .Status.InSync}}<span class="in-sync">IN SYNC</span>
    {{else}}<span class="drifted">DRIFTED ({{.Status.DriftCount}})</span>{{end}}
  </td>
  <td>
    {{if .Status.Error}}{{.Status.Error}}
    {{else}}<ul class="drift-list">{{range .Status.Drift}}<li>[{{.Status}}] {{.Kind}}/{{.Name}}</li>{{end}}</ul>{{end}}
  </td>
  <td>{{.Status.LastChecked}}</td>
</tr>
{{end}}
</table>
{{end}}
</body>
</html>`
