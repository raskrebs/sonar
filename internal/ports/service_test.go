package ports

import "testing"

func TestServiceName(t *testing.T) {
	tests := []struct {
		port int
		want string
	}{
		{53, "dns"},
		{631, "ipp"},
		{443, "https"},
		{5432, "postgresql"},
		{3000, ""},  // not a well-known port
		{99999, ""}, // out of range
	}
	for _, tt := range tests {
		if got := ServiceName(tt.port); got != tt.want {
			t.Errorf("ServiceName(%d) = %q, want %q", tt.port, got, tt.want)
		}
	}
}
