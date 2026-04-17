package ratelimit

import (
	"sync"
	"testing"
)

func TestLimiter_DisabledAlwaysAllows(t *testing.T) {
	t.Parallel()
	l := New(0)
	if l.Enabled() {
		t.Errorf("perMinute=0 must be disabled")
	}
	for i := 0; i < 1000; i++ {
		if !l.Allow("anyone") {
			t.Fatalf("disabled limiter denied on call %d", i)
		}
	}
}

func TestLimiter_NegativeDisables(t *testing.T) {
	t.Parallel()
	if New(-5).Enabled() {
		t.Errorf("negative perMinute must disable")
	}
}

func TestLimiter_BurstAllowsOneMinuteOfRequests(t *testing.T) {
	t.Parallel()
	l := New(10)
	// A brand-new bucket starts full, so the first 10 calls for a
	// caller should all be allowed before the bucket is empty.
	for i := 0; i < 10; i++ {
		if !l.Allow("alice") {
			t.Fatalf("first-minute burst denied on call %d", i)
		}
	}
	// The 11th call exhausts the bucket and should be denied.
	if l.Allow("alice") {
		t.Errorf("11th call should have been rate-limited")
	}
}

func TestLimiter_SeparateBucketsPerCaller(t *testing.T) {
	t.Parallel()
	l := New(2)
	if !l.Allow("alice") {
		t.Fatal("alice #1")
	}
	if !l.Allow("alice") {
		t.Fatal("alice #2")
	}
	// alice is now throttled; bob should still go through.
	if l.Allow("alice") {
		t.Errorf("alice should be throttled")
	}
	if !l.Allow("bob") {
		t.Errorf("bob should not inherit alice's bucket")
	}
}

func TestLimiter_PerMinute(t *testing.T) {
	t.Parallel()
	if got := New(42).PerMinute(); got != 42 {
		t.Errorf("PerMinute = %d, want 42", got)
	}
	if got := New(0).PerMinute(); got != 0 {
		t.Errorf("disabled PerMinute = %d, want 0", got)
	}
}

// Concurrent Allow calls must not race on the map. Exercises the
// per-caller lock and the atomic Allow inside rate.Limiter.
func TestLimiter_ConcurrentSafety(t *testing.T) {
	t.Parallel()
	l := New(1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = l.Allow("caller")
			}
		}(i)
	}
	wg.Wait()
}
