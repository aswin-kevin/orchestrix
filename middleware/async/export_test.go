package async

// NewLocalRunner returns the built-in Runner that New builds for cfg when
// cfg.Runner is nil, so tests outside the package can drive it directly.
func NewLocalRunner(cfg Config) (Runner, error) {
	cfg.Runner = nil
	m, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return m.runner, nil
}
