package orchestrix

// PresetStackNames returns the name of every slot of the preset's
// middleware stack, in order, for tests.
func PresetStackNames() []string {
	names := make([]string, len(presetStack))
	for i, e := range presetStack {
		names[i] = e.name
	}
	return names
}
