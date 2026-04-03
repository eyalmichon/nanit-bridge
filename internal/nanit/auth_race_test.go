package nanit

import (
	"context"
	"sync"
	"testing"
)

// TestTokenManagerBuildRequestContextRace proves that calling
// SetContext concurrently with buildRequest races on tm.ctx.
// Run with: go test -race -run TestTokenManagerBuildRequestContextRace ./internal/nanit/
func TestTokenManagerBuildRequestContextRace(t *testing.T) {
	tm := NewTokenManager("test@example.com", "pass", "")

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			tm.SetContext(context.Background())
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			// buildRequest reads tm.ctx directly (the bug).
			tm.buildRequest("GET", "http://localhost/test", nil)
		}
	}()

	wg.Wait()
}
