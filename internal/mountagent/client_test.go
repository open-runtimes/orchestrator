package mountagent

import "testing"

func TestClientRel(t *testing.T) {
	c := NewClient("http://agent", "t", "/workspace")
	for in, want := range map[string]string{
		"/workspace/code":         "code",
		"/workspace/a/b/":         "a/b",
		"/workspace/code.scratch": "code.scratch",
	} {
		got, err := c.rel(in)
		if err != nil || got != want {
			t.Errorf("rel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/workspace", "/etc/passwd", "/workspace/../etc", "/workspaceother/x"} {
		if got, err := c.rel(bad); err == nil {
			t.Errorf("rel(%q) = %q, want a refusal", bad, got)
		}
	}
}
