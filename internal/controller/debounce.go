package controller

import (
	"context"
	"time"
)

// runDebounced calls fn once per burst of triggers: after the first trigger it waits until no further trigger
// arrived for d, then calls fn with the number of coalesced triggers. It returns when ctx is done.
func runDebounced(ctx context.Context, trigger <-chan struct{}, d time.Duration, fn func(coalesced int)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
		}
		n := 1
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
				timer.Reset(d)
			case <-timer.C:
				break wait
			}
		}
		fn(n)
	}
}
