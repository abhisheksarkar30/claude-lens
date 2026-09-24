package consumer

import (
	"sync"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
)

// SetAccounts swaps the list while the loop resolves; -race is the assertion.
func TestSetAccountsIsRaceCleanAndTakesEffect(t *testing.T) {
	c := New(nil, nil, []config.Account{{Name: "old", BillingMode: "subscription"}})
	if name, _ := c.resolveAccountNow("oauth"); name != "old" {
		t.Fatalf("before the swap resolved %q, want old", name)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.resolveAccountNow("oauth")
			}
		}
	}()
	for i := 0; i < 200; i++ {
		c.SetAccounts([]config.Account{{Name: "new", BillingMode: "subscription"}})
	}
	close(stop)
	wg.Wait()

	if name, mode := c.resolveAccountNow("oauth"); name != "new" || mode != "subscription" {
		t.Fatalf("after the swap resolved %q/%q, want new/subscription", name, mode)
	}
}
