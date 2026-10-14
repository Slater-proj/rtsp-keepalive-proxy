package proxy

import (
	"context"
	"testing"
	"time"
)

func TestHostGateSerialisesAndSpaces(t *testing.T) {
	g := gateFor("gate-test:554")
	ctx := context.Background()

	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	got := make(chan time.Time, 1)
	go func() {
		if err := g.acquire(ctx); err == nil {
			got <- time.Now()
			g.releaseAfter(0)
		}
	}()

	select {
	case <-got:
		t.Fatal("second handshake entered while the first one is running")
	case <-time.After(100 * time.Millisecond):
	}

	released := time.Now()
	g.releaseAfter(200 * time.Millisecond)
	select {
	case at := <-got:
		if d := at.Sub(released); d < 150*time.Millisecond {
			t.Errorf("second handshake started %s after the first, want >= the spacing", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second handshake never started")
	}
}

func TestHostGateHonoursContext(t *testing.T) {
	g := gateFor("gate-test-ctx:554")
	if err := g.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer g.releaseAfter(0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.acquire(ctx); err == nil {
		t.Fatal("acquire must give up when the context ends")
	}
}

func TestGatesAreIndependentPerHost(t *testing.T) {
	a, b := gateFor("gate-a:554"), gateFor("gate-b:554")
	if err := a.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.releaseAfter(0)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		t.Fatal("another camera must not wait for this one")
	}
	b.releaseAfter(0)
}
