package api

import (
	"testing"
	"time"
)

// A refusal's wait has to cover the refill the request actually needs. It used
// to be a constant second, which is short of what a paged read costs at the
// default rate: a client that waited exactly as told was refused again.
func TestLimiterReportsTheRefillWait(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newLimiter(DefaultAPIRate, DefaultAPIBurst)
	l.now = func() time.Time { return now }

	// Spend most of the bucket: two paged reads of 2000 entries (22 tokens each).
	for i := 0; i < 2; i++ {
		if ok, _ := l.allowN("c", 22); !ok {
			t.Fatalf("read %d inside the burst was refused", i+1)
		}
	}
	ok, wait := l.allowN("c", 22) // 16 tokens left, 22 needed
	if ok {
		t.Fatal("a read past the burst was allowed")
	}
	want := time.Duration(6.0 / DefaultAPIRate * float64(time.Second))
	if wait != want {
		t.Fatalf("wait %v, want %v", wait, want)
	}
	// Waiting exactly that long is enough.
	now = now.Add(wait)
	if ok, _ := l.allowN("c", 22); !ok {
		t.Fatal("refused again after waiting the reported time")
	}
}

func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "1"},
		{300 * time.Millisecond, "1"},
		{time.Second, "1"},
		{1100 * time.Millisecond, "2"},
		{3 * time.Second, "3"},
	} {
		if got := retryAfterSeconds(c.d); got != c.want {
			t.Errorf("retryAfterSeconds(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
