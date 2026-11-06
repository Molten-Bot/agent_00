package library

// Names returns the ordered list of callable library task names. Production
// code reads Summaries directly; tests use this as a compact assertion helper.
func (c Catalog) Names() []string {
	summaries := c.Summaries()
	if len(summaries) == 0 {
		return nil
	}
	out := make([]string, 0, len(summaries))
	for _, task := range summaries {
		out = append(out, task.Name)
	}
	return out
}
