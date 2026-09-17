package drift

import "testing"

func TestDiffDesiredVsLive_IdenticalMaps(t *testing.T) {
	desired := map[string]interface{}{"name": "demo", "replicas": float64(2)}
	live := map[string]interface{}{"name": "demo", "replicas": float64(2)}

	if diffs := diffDesiredVsLive(desired, live, ""); len(diffs) != 0 {
		t.Fatalf("expected no diffs, got %+v", diffs)
	}
}

func TestDiffDesiredVsLive_IgnoresExtraLiveFields(t *testing.T) {
	// The cluster/API server adds fields Git never mentioned (defaults,
	// server-managed bookkeeping) — those must never count as drift.
	desired := map[string]interface{}{"name": "demo"}
	live := map[string]interface{}{"name": "demo", "uid": "abc-123", "somethingElse": true}

	if diffs := diffDesiredVsLive(desired, live, ""); len(diffs) != 0 {
		t.Fatalf("expected no diffs when live has extra fields, got %+v", diffs)
	}
}

func TestDiffDesiredVsLive_MissingFieldInLive(t *testing.T) {
	desired := map[string]interface{}{"replicas": float64(3)}
	live := map[string]interface{}{}

	diffs := diffDesiredVsLive(desired, live, "")
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %+v", diffs)
	}
	if diffs[0].path != "replicas" || diffs[0].live != nil {
		t.Errorf("unexpected diff: %+v", diffs[0])
	}
}

func TestDiffDesiredVsLive_MismatchedScalar(t *testing.T) {
	desired := map[string]interface{}{"replicas": float64(3)}
	live := map[string]interface{}{"replicas": float64(5)}

	diffs := diffDesiredVsLive(desired, live, "")
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %+v", diffs)
	}
	if diffs[0].desired != float64(3) || diffs[0].live != float64(5) {
		t.Errorf("unexpected diff values: %+v", diffs[0])
	}
}

func TestDiffDesiredVsLive_NestedMapIgnoresExtraLiveFields(t *testing.T) {
	desired := map[string]interface{}{
		"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "demo"}},
	}
	live := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels":          map[string]interface{}{"app": "demo"},
			"resourceVersion": "999",
		},
	}

	if diffs := diffDesiredVsLive(desired, live, ""); len(diffs) != 0 {
		t.Fatalf("expected no diffs, got %+v", diffs)
	}
}

func TestDiffDesiredVsLive_IgnoredMetadataFieldsNeverCountAsDrift(t *testing.T) {
	desired := map[string]interface{}{
		"metadata": map[string]interface{}{"name": "demo", "resourceVersion": "1"},
	}
	live := map[string]interface{}{
		"metadata": map[string]interface{}{"name": "demo", "resourceVersion": "999"},
	}

	if diffs := diffDesiredVsLive(desired, live, ""); len(diffs) != 0 {
		t.Fatalf("resourceVersion mismatch should be ignored, got %+v", diffs)
	}
}

func TestDiffDesiredVsLive_TopLevelStatusIgnored(t *testing.T) {
	desired := map[string]interface{}{"status": map[string]interface{}{"phase": "Running"}}
	live := map[string]interface{}{"status": map[string]interface{}{"phase": "Pending"}}

	if diffs := diffDesiredVsLive(desired, live, ""); len(diffs) != 0 {
		t.Fatalf("status should be ignored entirely, got %+v", diffs)
	}
}

// Regression test for the false-positive-drift bug found while testing
// against a real cluster: the API server adds defaults inside list
// elements (imagePullPolicy, resources, etc. on a container) — those must
// not count as drift, the same way extra fields on a plain map don't.
func TestDiffDesiredVsLive_ListOfMapsIgnoresExtraLiveFields(t *testing.T) {
	desired := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "nginx:1.25"},
			},
		},
	}
	live := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"name":            "app",
					"image":           "nginx:1.25",
					"imagePullPolicy": "IfNotPresent",
					"resources":       map[string]interface{}{},
				},
			},
		},
	}

	if diffs := diffDesiredVsLive(desired, live, ""); len(diffs) != 0 {
		t.Fatalf("expected no diffs, got %+v", diffs)
	}
}

func TestDiffDesiredVsLive_ListOfMapsDetectsRealMismatch(t *testing.T) {
	desired := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "nginx:1.25"},
			},
		},
	}
	live := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app", "image": "nginx:1.24"},
			},
		},
	}

	diffs := diffDesiredVsLive(desired, live, "")
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %+v", diffs)
	}
	if wantPath := "spec.containers[0].image"; diffs[0].path != wantPath {
		t.Errorf("path = %q, want %q", diffs[0].path, wantPath)
	}
}

func TestDiffDesiredVsLive_ListLengthMismatch(t *testing.T) {
	desired := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app"},
				map[string]interface{}{"name": "sidecar"},
			},
		},
	}
	live := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "app"},
			},
		},
	}

	diffs := diffDesiredVsLive(desired, live, "")
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff for a length mismatch, got %+v", diffs)
	}
	if wantPath := "spec.containers"; diffs[0].path != wantPath {
		t.Errorf("path = %q, want %q", diffs[0].path, wantPath)
	}
}

func TestResult_HasDrift(t *testing.T) {
	inSync := Result{Entries: []Entry{{Status: StatusInSync}}}
	if inSync.HasDrift() {
		t.Error("expected HasDrift() == false when every entry is IN SYNC")
	}

	drifted := Result{Entries: []Entry{{Status: StatusInSync}, {Status: StatusMissing}}}
	if !drifted.HasDrift() {
		t.Error("expected HasDrift() == true when any entry is not IN SYNC")
	}
}

func TestRemediationResult_HasErrors(t *testing.T) {
	clean := RemediationResult{Created: 1}
	if clean.HasErrors() {
		t.Error("expected HasErrors() == false with no Errors entries")
	}

	failed := RemediationResult{Errors: []string{"something went wrong"}}
	if !failed.HasErrors() {
		t.Error("expected HasErrors() == true with an Errors entry")
	}
}
