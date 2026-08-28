package bus

import (
	"testing"
	"time"
)

func TestPublishFanOut(t *testing.T) {
	b := New()
	a, ca := b.Subscribe("x", 4)
	defer ca()
	c, cc := b.Subscribe("x", 4)
	defer cc()

	b.Publish("x", 42)

	for i, ch := range []<-chan Envelope{a, c} {
		select {
		case env := <-ch:
			if env.Channel != "x" || env.Payload.(int) != 42 || env.At.IsZero() {
				t.Fatalf("subscriber %d got %+v", i, env)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d got nothing", i)
		}
	}
}

func TestPublishNonBlockingWhenFull(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("x", 1)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish("x", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
	if env := <-ch; env.Payload.(int) != 0 {
		t.Fatalf("first buffered payload = %v, want 0", env.Payload)
	}
}

func TestUnsubscribeIdempotentAndSafe(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("x", 1)
	cancel()
	cancel() // 幂等
	b.Publish("x", 1)
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after unsubscribe")
	}
}

func TestOtherChannelNotDelivered(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("a", 1)
	defer cancel()
	b.Publish("b", 1)
	select {
	case env := <-ch:
		t.Fatalf("channel a got %+v", env)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPublishToNoSubscribers(t *testing.T) {
	New().Publish("nobody", "listening") // 不应 panic
}
