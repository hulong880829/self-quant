package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/signing"
)

func TestPostContextNotConnected(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	_, err := s.PostContext(context.Background(), "action", map[string]any{})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err=%v, want ErrNotConnected", err)
	}
}

func TestPostActionContextTimeout(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	ctx := context.Background()
	if err := s.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	timeoutCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := s.PostActionContext(timeoutCtx, map[string]any{"type": "order"}, signing.SignatureResult{}, 1, "")
	if !errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("err=%v, want ErrRequestTimeout", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("timeout waited too long: %s", time.Since(started))
	}
}

func TestPostActionContextCanceled(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	if err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.PostActionContext(ctx, map[string]any{"type": "order"}, signing.SignatureResult{}, 1, "")
	if !errors.Is(err, ErrRequestCanceled) {
		t.Fatalf("err=%v, want ErrRequestCanceled", err)
	}
}

func TestPostActionContextDisconnectUnblocksPending(t *testing.T) {
	srv := newFakeWS(t)
	s := streamForURL(t, srv.URL)
	if err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := s.PostActionContext(
			context.Background(),
			map[string]any{"type": "order"},
			signing.SignatureResult{},
			1,
			"",
		)
		errCh <- err
	}()

	select {
	case <-srv.Received:
	case <-time.After(time.Second):
		t.Fatal("post was not sent")
	}

	started := time.Now()
	s.handleDisconnect()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrConnectionLost) {
			t.Fatalf("err=%v, want ErrConnectionLost", err)
		}
		if time.Since(started) > time.Second {
			t.Fatalf("disconnect waited too long: %s", time.Since(started))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending POST was not failed on disconnect")
	}
}

func TestPostActionContextDoesNotResendAfterDisconnect(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	if err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	errCh := make(chan error, 1)
	go func() {
		_, err := s.PostActionContext(
			context.Background(),
			map[string]any{"type": "order"},
			signing.SignatureResult{},
			1,
			"",
		)
		errCh <- err
	}()
	select {
	case <-srv.Received:
	case <-time.After(time.Second):
		t.Fatal("post was not sent")
	}
	s.handleDisconnect()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrConnectionLost) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending POST not failed")
	}
	select {
	case <-srv.Received:
		t.Fatal("disconnect resent an action")
	case <-time.After(50 * time.Millisecond):
	}
}
