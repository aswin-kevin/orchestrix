package backendtest

import (
	"testing"
	"time"
)

// TestOptionsNegatives pins ModTimeResolution: zero keeps the default gap, a
// positive value widens it and a negative value is an error, which RunWith
// reports with t.Fatal.
func TestOptionsNegatives(t *testing.T) {
	for _, tt := range []struct {
		res     time.Duration
		want    time.Duration
		wantErr string
	}{
		{0, mtimeGap, ""},
		{time.Second, time.Second + resolutionSlack, ""},
		{-1, 0, "backendtest: ModTimeResolution is negative"},
	} {
		got, err := Options{ModTimeResolution: tt.res}.writeGap()
		if got != tt.want || errText(err) != tt.wantErr {
			t.Errorf("writeGap(%v) = %v, %v; want %v, %q", tt.res, got, err, tt.want, tt.wantErr)
		}
	}
}

// errText returns err's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
