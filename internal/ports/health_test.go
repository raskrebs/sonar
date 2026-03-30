package ports

import (
	"errors"
	"net/url"
	"testing"
)

func TestIsConnectionRefused(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "connection refused",
			err:  errors.New("connection refused"),
			want: true,
		},
		{
			name: "wrapped in url.Error",
			err:  &url.Error{Op: "Get", URL: "http://localhost:9999", Err: errors.New("connection refused")},
			want: true,
		},
		{
			name: "timeout error",
			err:  errors.New("i/o timeout"),
			want: false,
		},
		{
			name: "unrelated error",
			err:  errors.New("something else went wrong"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConnectionRefused(tt.err); got != tt.want {
				t.Errorf("isConnectionRefused(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
