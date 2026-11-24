package workflow

import (
	"os"
	"testing"
)

func TestCustomOwnershipPreservesDefaultsUntilEnabled(t *testing.T) {
	t.Parallel()
	w := definition("override")
	w.Enabled = false
	e, _, _ := testEngine(t, w)
	if e.Owns(w.Repository, "issue.*") {
		t.Fatal("disabled migration draft replaced configured defaults")
	}
	doc, _ := e.Store.Load()
	doc.Workflows[0].Enabled = true
	if _, err := e.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	if !e.Owns("https://github.com/ACME/repo.git", "issue.*") || !e.Owns(w.Repository, "issue.labeled") {
		t.Fatal("enabled custom workflow did not own matching defaults")
	}
	if e.Owns(w.Repository, "pr.review_requested") || e.Owns("acme/other", "issue.*") {
		t.Fatal("custom workflow replaced unrelated defaults")
	}
	e.Enabled = func() bool { return false }
	if e.Owns(w.Repository, "issue.*") {
		t.Fatal("disabled builder replaced configured defaults")
	}
	e.Enabled = func() bool { return true }
	if err := os.WriteFile(e.Store.Path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if !e.Owns(w.Repository, "issue.*") {
		t.Fatal("unreadable ownership store allowed competing automation")
	}
}
