package engine

// Requirements is the minimum version of every registered engine that
// declares one: what `viiwork --engine-requirements` prints, so a node staging
// a release reads the requirement from the signed binary itself.
func Requirements() map[string]string {
	out := map[string]string{}
	for _, name := range Names() {
		e, _ := Lookup(name)
		if v, ok := e.(Versioner); ok && v.MinVersion() != "" {
			out[name] = v.MinVersion()
		}
	}
	return out
}
