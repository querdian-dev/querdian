package controller

import (
	"context"
	"time"
)

// runDebounced calls fn once per burst of triggers: after the first trigger it waits until no further trigger
// arrived for d, then calls fn with the number of coalesced triggers. A burst never waits longer than maxWait
// after its first trigger, so a stream of events cannot delay a rebuild without limit; maxWait <= 0 means no
// limit. It returns when ctx is done.
func runDebounced(ctx context.Context, trigger <-chan struct{}, d, maxWait time.Duration, fn func(coalesced int)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
		}
		n := 1
		first := time.Now()
		timer := time.NewTimer(d)
	wait:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-trigger:
				n++
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				wait := d
				if maxWait > 0 {
					wait = min(d, max(0, maxWait-time.Since(first)))
				}
				timer.Reset(wait)
			case <-timer.C:
				break wait
			}
		}
		fn(n)
	}
}
