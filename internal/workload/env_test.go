package workload

import (
	"maps"
	"testing"
)

func TestNormalizeEnv(t *testing.T) {
	t.Run("trims names", func(t *testing.T) {
		env := map[string]string{" FOO\n": "1", "BAR": " padded value "}
		if err := NormalizeEnv(env); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]string{"FOO": "1", "BAR": " padded value "}
		if !maps.Equal(env, want) {
			t.Errorf("got %v, want %v", env, want)
		}
	})

	t.Run("nil and empty are fine", func(t *testing.T) {
		if err := NormalizeEnv(nil); err != nil {
			t.Errorf("unexpected error for nil: %v", err)
		}
		if err := NormalizeEnv(map[string]string{}); err != nil {
			t.Errorf("unexpected error for empty: %v", err)
		}
	})

	t.Run("rejects blank names", func(t *testing.T) {
		for _, name := range []string{"", " ", "\n"} {
			if err := NormalizeEnv(map[string]string{name: "v"}); err == nil {
				t.Errorf("expected error for name %q", name)
			}
		}
	})

	t.Run("rejects collisions after trimming", func(t *testing.T) {
		if err := NormalizeEnv(map[string]string{"FOO ": "a", "FOO": "b"}); err == nil {
			t.Error("expected collision error")
		}
	})
}
