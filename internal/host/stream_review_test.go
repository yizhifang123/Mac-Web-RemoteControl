package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHelper writes a shell "capture helper" that records its argv and everything it
// reads on stdin, and never emits a frame (so stream() blocks until ctx ends).
func fakeHelper(t *testing.T) (path, argsFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	stdinFile = filepath.Join(dir, "stdin")
	path = filepath.Join(dir, "helper.sh")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\nexec cat > " + stdinFile + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

// View-only: the helper is NOT started with --input, but its stdin is open and a
// keyframe request reaches it as exactly "kf\n". After the session ends, writes are
// dropped, not sent to the dead helper.
func TestReviewViewOnlyStreamCarriesOnlyKeyframeRequests(t *testing.T) {
	bin, argsFile, stdinFile := fakeHelper(t)
	s := &screenSource{helperPath: bin, helperSource: "test", inputMode: "off",
		cfg: captureConfig{width: 64, height: 64, fps: 30, bitrate: 1000, gopSeconds: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.stream(ctx, nil, nil) }()
	waitFor(t, "helper start", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stdin != nil && readFile(argsFile) != ""
	})
	if a := readFile(argsFile); strings.Contains(a, "--input") {
		t.Fatalf("view-only helper started with --input: %q", a)
	}
	s.requestKeyframe()
	waitFor(t, "kf on helper stdin", func() bool { return readFile(stdinFile) == "kf\n" })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not return after cancel")
	}
	s.mu.Lock()
	left := s.stdin
	s.mu.Unlock()
	if left != nil {
		t.Fatal("stdin still set after the session ended")
	}
	s.requestKeyframe() // must be a harmless no-op
}

// screenSource outlives sessions: after session 1 ends, its pump must be gone and a
// move put during session 2 must reach session 2's helper, not the dead one.
func TestReviewPumpDoesNotOutliveSession(t *testing.T) {
	s := &screenSource{}
	var w1, w2 bufCloser
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { s.pumpMoves(ctx1, &w1); close(done1) }()
	s.writeMove(clientInput{T: "m", X: 0.1, Y: 0.1})
	waitFor(t, "move on session 1", func() bool {
		w1.mu.Lock()
		defer w1.mu.Unlock()
		return w1.Len() > 0
	})
	cancel1()
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpMoves leaked past its session")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go s.pumpMoves(ctx2, &w2)
	s.writeMove(clientInput{T: "r", DX: 2, DY: 3})
	waitFor(t, "move on session 2", func() bool {
		w2.mu.Lock()
		defer w2.mu.Unlock()
		return w2.String() == "r 2.0 3.0 0\n"
	})
	w1.mu.Lock()
	defer w1.mu.Unlock()
	if w1.String() != "m 0.10000 0.10000 0\n" {
		t.Fatalf("dead session's helper got extra writes: %q", w1.String())
	}
}

// Concurrent moves, input lines and keyframe requests: every line arrives whole, and
// no relative motion is lost between the slot and stdin.
func TestReviewConcurrentWritesNeverInterleaveOrLoseDeltas(t *testing.T) {
	out := &bufCloser{}
	s := &screenSource{stdin: out}
	ctx, cancel := context.WithCancel(context.Background())
	pumpDone := make(chan struct{})
	go func() { s.pumpMoves(ctx, out); close(pumpDone) }()
	var wg sync.WaitGroup
	const n = 2000
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.writeMove(clientInput{T: "r", DX: 1, DY: -1})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n/10; i++ {
			s.writeInput([]byte("k KeyA 0\n"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n/10; i++ {
			s.requestKeyframe()
		}
	}()
	wg.Wait()
	s.writeInput([]byte("x\n")) // flushes any pending move first
	cancel()
	<-pumpDone
	var sumX, keys, kfs int
	for _, ln := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		switch {
		case ln == "k KeyA 0":
			keys++
		case ln == "kf":
			kfs++
		case ln == "x":
		case strings.HasPrefix(ln, "r "):
			var dx, dy float64
			var f int
			if _, err := sscanR(ln, &dx, &dy, &f); err != nil || dx != -dy {
				t.Fatalf("mangled move line %q", ln)
			}
			sumX += int(dx)
		default:
			t.Fatalf("interleaved/garbled line %q", ln)
		}
	}
	if sumX != n || keys != n/10 || kfs != n/10 {
		t.Fatalf("sumX=%d keys=%d kfs=%d, want %d %d %d", sumX, keys, kfs, n, n/10, n/10)
	}
}

func sscanR(ln string, dx, dy *float64, f *int) (int, error) {
	return fmt.Sscanf(ln, "r %f %f %d", dx, dy, f)
}
