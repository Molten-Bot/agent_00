package workflow

import (
	"encoding/json"
	"os"
	"testing"
)

func TestBundledTemplatesMatchWorkflowContract(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../web/workflow/src/templates.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Version         int `json:"version"`
		WorkflowVersion int `json:"workflowVersion"`
		Templates       []struct {
			ID          string     `json:"id"`
			Name        string     `json:"name"`
			Description string     `json:"description"`
			Definition  Definition `json:"definition"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Version != 1 || catalog.WorkflowVersion != Version {
		t.Fatal("template schema version mismatch")
	}
	doc := Document{Version: catalog.WorkflowVersion}
	for _, template := range catalog.Templates {
		if template.ID != template.Definition.ID || template.Name == "" || template.Description == "" {
			t.Fatalf("invalid template metadata: %s", template.ID)
		}
		if template.Definition.Enabled || template.Definition.IncludeBacklog {
			t.Fatalf("template %s must require explicit activation", template.ID)
		}
		doc.Workflows = append(doc.Workflows, template.Definition)
	}
	if len(doc.Workflows) == 0 {
		t.Fatal("missing templates")
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
}
