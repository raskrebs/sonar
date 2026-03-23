package ports

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// HealthResult holds the outcome of probing a single port.
type HealthResult struct {
	Status     string
	StatusCode int
	Latency    time.Duration
}

// ProbeHealth performs an HTTP GET to localhost:port/path and classifies the result.
// If path is empty, "/" is used.
func ProbeHealth(port int, path string, timeout time.Duration) HealthResult {
	if path == "" {
		path = "/"
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	start := time.Now()
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d%s", port, path))
	latency := time.Since(start)

	if err != nil {
		if urlErr, ok := err.(*url.Error); ok && urlErr.Timeout() {
			return HealthResult{Status: "timeout", Latency: latency}
		}
		// Connection refused or other dial errors
		if isConnectionRefused(err) {
			return HealthResult{Status: "refused", Latency: latency}
		}
		// Anything else (e.g. non-HTTP server sending garbage)
		return HealthResult{Status: "non-http", Latency: latency}
	}
	defer resp.Body.Close()

	code := resp.StatusCode
	if code >= 200 && code < 400 {
		return HealthResult{Status: "healthy", StatusCode: code, Latency: latency}
	}
	return HealthResult{Status: "unhealthy", StatusCode: code, Latency: latency}
}

// isConnectionRefused checks whether the error chain contains a
// "connection refused" indication.
func isConnectionRefused(err error) bool {
	for err != nil {
		if e, ok := err.(*url.Error); ok {
			err = e.Err
			continue
		}
		// Check the string as a fallback — the stdlib wraps the
		// syscall error in various layers.
		if contains(fmt.Sprintf("%v", err), "connection refused") ||
			contains(err.Error(), "connection refused") {
			return true
		}
		break
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// EnrichHealth probes every port concurrently (max 10 at a time) and
// populates the Health* fields on each ListeningPort.
func EnrichHealth(pp []ListeningPort, timeout time.Duration) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for i := range pp {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			result := ProbeHealth(pp[idx].Port, "/", timeout)
			pp[idx].HealthStatus = result.Status
			pp[idx].HealthCode = result.StatusCode
			pp[idx].HealthLatency = result.Latency
		}(i)
	}
	wg.Wait()
}
