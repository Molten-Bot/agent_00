package localruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSchedulerSettingsCompatibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, settings string
		want           int
	}{
		{"legacy", `"dispatcher":{"max_parallel":3}`, 3},
		{"current", `"scheduler":{"max_parallel":2}`, 2},
		{"current wins", `"dispatcher":{"max_parallel":3},"scheduler":{"max_parallel":2}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			data := `{"version":"v1","agent_harness":"claude","custom":"preserved",` + tc.settings + `}`
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			init, err := LoadInit(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadRuntimeConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if init.Scheduler.MaxParallel != tc.want || cfg.Scheduler.MaxParallel != tc.want {
				t.Fatalf("max parallel: init=%d runtime=%d want=%d", init.Scheduler.MaxParallel, cfg.Scheduler.MaxParallel, tc.want)
			}
			if err := SaveRuntimeConfigReviewAndDefaultLibrarySettings(path, cfg.Init(), cfg.ReviewWatch, nil, nil); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(saved, &doc); err != nil {
				t.Fatal(err)
			}
			if _, ok := doc["dispatcher"]; ok {
				t.Fatal("saved retired scheduler key")
			}
			if string(doc["custom"]) != `"preserved"` || string(doc["agent_harness"]) != `"claude"` {
				t.Fatal("lost existing settings")
			}
			var scheduler SchedulerConfig
			if err := json.Unmarshal(doc["scheduler"], &scheduler); err != nil {
				t.Fatal(err)
			}
			if scheduler.MaxParallel != tc.want {
				t.Fatalf("saved max parallel=%d want=%d", scheduler.MaxParallel, tc.want)
			}
		})
	}
}
