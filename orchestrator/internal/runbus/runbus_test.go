package runbus

import (
	"sync"
	"testing"
)

// Subscribers that come and go (browsers closing their SSE stream) while
// activities publish must never make Publish send on a closed channel.
func TestPublishWhileUnsubscribing(t *testing.T) {
	b := New(1)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish("run", Event{Type: "step-progress"})
				}
			}
		}()
	}
	for i := 0; i < 5000; i++ {
		_, unsub := b.Subscribe("run")
		unsub()
	}
	close(stop)
	wg.Wait()
}

func TestPublishDropsOldestWhenFull(t *testing.T) {
	b := New(1)
	ch, unsub := b.Subscribe("run")
	defer unsub()
	b.Publish("run", Event{Type: "a"})
	b.Publish("run", Event{Type: "b"})
	if got := (<-ch).Type; got != "b" {
		t.Fatalf("got %q, want the newest event", got)
	}
}
