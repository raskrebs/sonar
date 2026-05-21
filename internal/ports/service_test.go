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

func TestRegisterServices(t *testing.T) {
	// Save and restore so we don't pollute other tests.
	orig := make(map[int]string, len(wellKnownServices))
	for k, v := range wellKnownServices {
		orig[k] = v
	}
	t.Cleanup(func() { wellKnownServices = orig })

	RegisterServices(map[int]string{
		9000: "php-fpm", // new port
		53:   "my-dns",  // override built-in
		1234: "",        // empty name — must be skipped
	})

	if got := ServiceName(9000); got != "php-fpm" {
		t.Errorf("ServiceName(9000) = %q, want php-fpm", got)
	}
	if got := ServiceName(53); got != "my-dns" {
		t.Errorf("ServiceName(53) = %q, want my-dns (override)", got)
	}
	if got := ServiceName(1234); got != "" {
		t.Errorf("ServiceName(1234) = %q, want empty (skipped)", got)
	}
}
