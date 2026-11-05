package dahua

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// TestOpenReentrantNoRace proves that two concurrent Open calls on the same
// transport cannot race on the t.ctrl/t.sub connection pointers.
//
// Without the Open() serialisation guard (the t.opening check-and-set) this
// fails under `go test -race`: both goroutines see t.opened==false, both dial,
// and both write t.ctrl (net.DialTimeout) concurrently. With the guard, only
// one Open proceeds and the second returns ErrOpenAlreadyInProgress, so the
// pointers are touched by a single goroutine.
//
// The "device" is a silent TCP listener: it accepts the connection (so
// DialTimeout succeeds and reaches the pointer assignment) but never speaks the
// login protocol, so Open blocks in login() until the short timeout and then
// returns an error. That is enough to exercise the concurrent dial writes the
// guard must prevent; no protocol mock is needed.
func TestOpenReentrantNoRace(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept connections but never respond, keeping Open blocked in login()
	// and widening the window where a concurrent dial would race.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	tr := NewNetSDKTransport(ln.Addr().String(), "user", "pass", 0, 200*time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(2)
	var err1, err2 error
	go func() {
		defer wg.Done()
		err1 = tr.Open(&core.Codec{Name: core.CodecPCMA, ClockRate: 8000})
	}()
	go func() {
		defer wg.Done()
		err2 = tr.Open(&core.Codec{Name: core.CodecPCMA, ClockRate: 8000})
	}()
	wg.Wait()

	// The real assertion is that `go test -race` reports no data race. The two
	// non-nil checks only confirm both Open paths were actually exercised
	// regardless of which goroutine won the in-flight slot (the assertion is
	// order-independent, so the test is not flaky).
	if err1 == nil {
		t.Fatal("expected first Open to fail against a silent listener")
	}
	if err2 == nil {
		t.Fatal("expected second Open to be rejected by the guard")
	}
}

// TestSendTextStalledPeerDoesNotBlock: a camera that stops reading the control
// connection must not pin ctrlMu forever, or Close (setTalkState + DeleteObject)
// hangs the talk teardown. net.Pipe has no buffer, so a peer that never reads
// stalls every write.
func TestSendTextStalledPeerDoesNotBlock(t *testing.T) {
	old := ctrlWriteTimeout
	ctrlWriteTimeout = 50 * time.Millisecond
	defer func() { ctrlWriteTimeout = old }()

	ours, theirs := net.Pipe()
	defer ours.Close()
	defer theirs.Close()

	tr := NewNetSDKTransport("127.0.0.1:37777", "u", "p", 0, time.Second)
	tr.ctrl = ours

	done := make(chan error, 1)
	go func() { done <- tr.sendText(ours, []string{"Method:Test"}) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("write to a stalled peer reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendText blocked on a stalled peer")
	}

	// The mutex must be free again: a second write also returns promptly.
	go func() { done <- tr.sendText(ours, []string{"Method:Test"}) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctrlMu still held after the bounded write")
	}
}
