package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dynatrace-oss/dtctl/pkg/apply"
	"github.com/dynatrace-oss/dtctl/pkg/client"
	"github.com/dynatrace-oss/dtctl/pkg/resources/document"
	"github.com/dynatrace-oss/dtctl/pkg/stability"
)

// newEnvShareMock builds an httptest server that mocks the minimal Document
// Service endpoints `apply --share-environment` touches. It returns deterministic
// behaviour per documentID so tests can drive success/failure paths without a
// real API. `fail` is the set of document IDs whose POST should return a 500.
func newEnvShareMock(t *testing.T, fail map[string]bool) (*httptest.Server, map[string]*int64) {
	t.Helper()
	counts := map[string]*int64{}
	var mu sync.Mutex
	mux := http.NewServeMux()

	mux.HandleFunc("/platform/document/v1/environment-shares", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			// No existing shares — forces create path.
			_ = json.NewEncoder(w).Encode(document.EnvironmentShareList{Shares: nil, TotalCount: 0})
			return
		}
		if r.Method == http.MethodPost {
			var body document.CreateEnvironmentShareRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if fail[body.DocumentID] {
				http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
				return
			}
			mu.Lock()
			if _, ok := counts[body.DocumentID]; !ok {
				var n int64
				counts[body.DocumentID] = &n
			}
			mu.Unlock()
			atomic.AddInt64(counts[body.DocumentID], 1)
			_ = json.NewEncoder(w).Encode(document.EnvironmentShare{
				ID: "share-" + body.DocumentID, DocumentID: body.DocumentID, Access: []string{"read"},
			})
		}
	})

	// :makePublic, counted under "public:<id>". --share-environment sends it
	// without reading the document's metadata first: the action does nothing
	// when the document is already public.
	mux.HandleFunc("/platform/document/v1/documents/", func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/platform/document/v1/documents/"), ":makePublic")
		if r.Method != http.MethodPost || !ok {
			t.Errorf("unexpected document request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		key := "public:" + id
		if _, ok := counts[key]; !ok {
			var n int64
			counts[key] = &n
		}
		mu.Unlock()
		atomic.AddInt64(counts[key], 1)
		w.WriteHeader(http.StatusNoContent)
	})

	return httptest.NewServer(mux), counts
}

func newEnvShareClient(t *testing.T, srv *httptest.Server) *client.Client {
	t.Helper()
	c, err := client.NewForTesting(srv.URL, "test-token")
	if err != nil {
		t.Fatalf("client.NewForTesting: %v", err)
	}
	return c
}

func TestEnsureEnvironmentShareForResults_SkipsNonDocuments(t *testing.T) {
	srv, counts := newEnvShareMock(t, nil)
	defer srv.Close()
	c := newEnvShareClient(t, srv)

	results := []apply.ApplyResult{
		&apply.NotebookApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "nb-1", ResourceType: "notebook"}},
		&apply.WorkflowApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "wf-1", ResourceType: "workflow"}},
		&apply.SLOApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "slo-1", ResourceType: "slo"}},
		&apply.DashboardApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "db-1", ResourceType: "dashboard"}},
		&apply.DocumentApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "lp-1", ResourceType: "launchpad"}},
	}
	if err := ensureEnvironmentShareForResults(context.Background(), c, results, "read"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Both halves, for every document type: the link and isPrivate=false.
	for _, key := range []string{"nb-1", "public:nb-1", "db-1", "public:db-1", "lp-1", "public:lp-1"} {
		if got := counts[key]; got == nil || atomic.LoadInt64(got) != 1 {
			t.Errorf("expected %s once, counts=%v", key, counts)
		}
	}
	if got := counts["nb-1"]; got == nil || atomic.LoadInt64(got) != 1 {
		t.Errorf("expected notebook nb-1 to be shared once, counts=%v", counts)
	}
	if got := counts["db-1"]; got == nil || atomic.LoadInt64(got) != 1 {
		t.Errorf("expected dashboard db-1 to be shared once, counts=%v", counts)
	}
	if _, ok := counts["wf-1"]; ok {
		t.Errorf("workflow wf-1 must not be shared")
	}
	if _, ok := counts["slo-1"]; ok {
		t.Errorf("slo slo-1 must not be shared")
	}
}

func TestEnsureEnvironmentShareForResults_SkipsEmptyID(t *testing.T) {
	srv, counts := newEnvShareMock(t, nil)
	defer srv.Close()
	c := newEnvShareClient(t, srv)

	results := []apply.ApplyResult{
		&apply.NotebookApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "", ResourceType: "notebook"}},
	}
	if err := ensureEnvironmentShareForResults(context.Background(), c, results, "read"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("expected no share calls for empty ID, got %v", counts)
	}
}

func TestEnsureEnvironmentShareForResults_ContinuesAfterFailure(t *testing.T) {
	// First doc fails; remaining documents must still be attempted and the
	// combined error must reference the failing ID.
	srv, counts := newEnvShareMock(t, map[string]bool{"nb-bad": true})
	defer srv.Close()
	c := newEnvShareClient(t, srv)

	results := []apply.ApplyResult{
		&apply.NotebookApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "nb-bad", ResourceType: "notebook"}},
		&apply.DashboardApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "db-ok", ResourceType: "dashboard"}},
	}
	err := ensureEnvironmentShareForResults(context.Background(), c, results, "read")
	if err == nil {
		t.Fatal("expected error from failing share")
	}
	if !strings.Contains(err.Error(), "nb-bad") {
		t.Errorf("error should reference failing document, got %q", err.Error())
	}
	if got := counts["db-ok"]; got == nil || atomic.LoadInt64(got) != 1 {
		t.Errorf("second document should still be shared after first failure, counts=%v", counts)
	}
}

func TestEnsureEnvironmentShareForResults_MultipleFailuresCombined(t *testing.T) {
	srv, _ := newEnvShareMock(t, map[string]bool{"nb-1": true, "db-1": true})
	defer srv.Close()
	c := newEnvShareClient(t, srv)

	results := []apply.ApplyResult{
		&apply.NotebookApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "nb-1", ResourceType: "notebook"}},
		&apply.DashboardApplyResult{ApplyResultBase: apply.ApplyResultBase{ID: "db-1", ResourceType: "dashboard"}},
	}
	err := ensureEnvironmentShareForResults(context.Background(), c, results, "read")
	if err == nil {
		t.Fatal("expected combined error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "2 documents failed") {
		t.Errorf("combined error should count failures, got %q", msg)
	}
	if !strings.Contains(msg, "nb-1") || !strings.Contains(msg, "db-1") {
		t.Errorf("combined error should reference both failing IDs, got %q", msg)
	}
}

func TestApplyCmd_ShareEnvironmentFlagValidation(t *testing.T) {
	// The flag validates up-front before any HTTP work, so we exercise it by
	// setting args on a fresh command tree and asserting the RunE error.
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"", false},
		{"read", false},
		{"read-write", false},
		{"write", true},
		{"bogus", true},
		{"READ", true},
	}
	for _, tt := range tests {
		t.Run("value="+tt.value, func(t *testing.T) {
			// Reach into the flag value directly; RunE reads it via cmd.Flags().GetString.
			f := applyCmd.Flags().Lookup("share-environment")
			if f == nil {
				t.Fatal("--share-environment flag not registered")
			}
			orig := f.Value.String()
			defer func() { _ = f.Value.Set(orig) }()
			if err := f.Value.Set(tt.value); err != nil {
				t.Fatalf("Set: %v", err)
			}

			// Invoke the validation directly — the RunE's first few lines are
			// flag reads + value check. We reproduce that narrow check here to
			// avoid needing a file/client for a pure validation test.
			val := f.Value.String()
			err := validateShareEnvironmentValue(val)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateShareEnvironmentValue(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

// TestApplyCmd_ShareEnvironmentDeprecated: --share-environment keeps its
// shape (a string, bare means 'read') and carries a complete deprecation
// record naming the replacement, which hides it from help and lets
// DTCTL_NO_DEPRECATED refuse it.
func TestApplyCmd_ShareEnvironmentDeprecated(t *testing.T) {
	f := applyCmd.Flags().Lookup("share-environment")
	if f == nil {
		t.Fatal("--share-environment flag not registered")
	}
	if f.Value.Type() != "string" || f.NoOptDefVal != "read" {
		t.Errorf("flag shape = %s (bare %q), want string with bare 'read' as on main", f.Value.Type(), f.NoOptDefVal)
	}
	d, ok := stability.FlagDeprecationOf(applyCmd, "share-environment")
	if !ok {
		t.Fatal("--share-environment carries no deprecation record")
	}
	if d.Since == "" || d.RemoveIn == "" || !strings.Contains(d.Replacement, "share document") {
		t.Errorf("deprecation = %+v, want since, remove-in, and the share document replacement", d)
	}
	if f.Deprecated == "" || !f.Hidden {
		t.Errorf("flag is not deprecated in pflag (deprecated %q, hidden %v)", f.Deprecated, f.Hidden)
	}
}
