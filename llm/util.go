package llm

// firstNonEmpty returns the first non-empty string, or "" if none.
func firstNonEmpty[T ~string](values ...T) T {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstNonZero returns the first non-zero int, or 0 if none.
func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
