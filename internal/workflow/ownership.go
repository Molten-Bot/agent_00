package workflow

import "strings"

// Owns reports whether enabled custom automation replaces a configured default
// for this repository and event. A trailing .* matches a trigger family.
// An unreadable definition store fails closed to avoid duplicate dispatch.
func (e *Engine) Owns(repo, event string) bool {
	if e == nil || e.Enabled == nil || !e.Enabled() {
		return false
	}
	if e.Allowed != nil && !e.Allowed(repo) {
		return true
	}
	if e.Store == nil {
		return true
	}
	doc, err := e.Store.Load()
	if err != nil {
		return true
	}
	// A paused or deleted migrated rule must not reactivate legacy automation.
	if doc.ConfigurationMigrated && strings.HasPrefix(event, "issue.") {
		for _, migrated := range doc.MigratedRepositories {
			if CanonicalRepository(migrated) == CanonicalRepository(repo) {
				return true
			}
		}
	}
	for _, w := range doc.Workflows {
		if !w.Enabled || CanonicalRepository(w.Repository) != CanonicalRepository(repo) {
			continue
		}
		if w.Trigger.Event == event || (strings.HasSuffix(event, ".*") && strings.HasPrefix(w.Trigger.Event, strings.TrimSuffix(event, "*"))) {
			return true
		}
	}
	return false
}
