package nanit

import "testing"

func TestDirOf(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/a/b/c", "/a/b"},
		{"/a", ""},
		{"file", "."},
		{"a/b", "a"},
	}

	for _, tc := range tests {
		got := dirOf(tc.in)
		if got != tc.want {
			t.Fatalf("dirOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
