package control

import (
	"math"
	"testing"
)

func TestConfigRevisionFromEnvDefaultsToOne(t *testing.T) {
	t.Setenv("AUTOSTREAM_CONFIG_REVISION", "")

	got, err := ConfigRevisionFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("revision = %d, want 1", got)
	}
}

func TestConfigRevisionFromEnvAcceptsPositiveIntegerBoundaries(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int64
	}{
		{raw: "1", want: 1},
		{raw: "42", want: 42},
		{raw: "9223372036854775807", want: math.MaxInt64},
	} {
		t.Run(test.raw, func(t *testing.T) {
			t.Setenv("AUTOSTREAM_CONFIG_REVISION", test.raw)
			got, err := ConfigRevisionFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("revision = %d, want %d", got, test.want)
			}
		})
	}
}

func TestConfigRevisionFromEnvRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{"0", "01", "-1", "+1", "1.5", " 1", "1 ", "invalid", "9223372036854775808"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("AUTOSTREAM_CONFIG_REVISION", raw)
			if _, err := ConfigRevisionFromEnv(); err == nil {
				t.Fatalf("ConfigRevisionFromEnv() accepted %q", raw)
			}
		})
	}
}
