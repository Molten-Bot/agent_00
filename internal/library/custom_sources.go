package library

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CustomSourcesDirEnv overrides where LoadCustomizedCatalog looks for
// generated custom library sources (see internal/customlibrary). Each
// immediate subdirectory of this root that contains a "generated"
// subdirectory of *.json files is merged into the catalog, alongside the
// built-in dir.
const CustomSourcesDirEnv = "HARNESS_LIBRARY_CUSTOM_DIR"

const defaultCustomSourcesRoot = "./.agent00/library-sources"

// CustomSourcesRoot resolves where custom library source catalogs live.
func CustomSourcesRoot() string {
	if v := strings.TrimSpace(os.Getenv(CustomSourcesDirEnv)); v != "" {
		return v
	}
	return defaultCustomSourcesRoot
}

// LoadCustomizedCatalog loads the built-in catalog from dir, then merges in
// every configured custom library source's generated catalog. A custom
// source that fails to load (not yet synced, or left mid-write) is skipped
// rather than failing the whole catalog, so one broken company source
// cannot take down the built-in library tasks. internal/customlibrary
// validates a source's content before it ever reaches disk here, so a skip
// at this layer should only happen transiently.
func LoadCustomizedCatalog(dir string) (Catalog, error) {
	return loadCustomizedCatalog(dir, true)
}

// LoadCustomLibraryCatalog merges every configured custom library source's
// generated catalog without the built-in ./library/*.json catalog. It backs
// the settings "default library" toggle, which hides built-in tasks from the
// picker without breaking already-scheduled recurring tasks that reference a
// default library task name: those keep resolving through
// LoadCustomizedCatalog regardless of the toggle.
func LoadCustomLibraryCatalog() (Catalog, error) {
	return loadCustomizedCatalog("", false)
}

func loadCustomizedCatalog(dir string, includeDefault bool) (Catalog, error) {
	catalog := Catalog{byName: map[string]TaskDefinition{}}
	if includeDefault {
		var err error
		catalog, err = LoadCatalog(dir)
		if err != nil {
			return Catalog{}, err
		}
	}

	root := CustomSourcesRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return catalog, nil
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		generatedDir := filepath.Join(root, name, "generated")
		if !isCatalogDir(generatedDir) {
			continue
		}
		extra, err := LoadCatalog(generatedDir)
		if err != nil {
			continue
		}
		for _, task := range extra.Tasks {
			if _, exists := catalog.byName[task.Name]; exists {
				continue
			}
			catalog.byName[task.Name] = task
			catalog.Tasks = append(catalog.Tasks, task)
		}
	}
	return catalog, nil
}
