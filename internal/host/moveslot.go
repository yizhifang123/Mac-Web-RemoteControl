package host

import (
	"math"
	"sync"
)

// moveSlot holds at most ONE pending mouse move. Moves come in far faster than they
// matter: only the latest absolute position counts, and relative deltas can be summed
// without losing motion. So instead of a queue that grows when the injector falls
// behind, put() merges into the slot and take() empties it.
type moveSlot struct {
	mu      sync.Mutex
	pending bool
	m       clientInput
	wake    chan struct{}
}

func (s *moveSlot) wakeCh() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	return s.wake
}

// put merges a validated "m" or "r" move into the slot and wakes the pump.
func (s *moveSlot) put(m clientInput) {
	if m.T != "m" && m.T != "r" {
		return
	}
	w := s.wakeCh()
	s.mu.Lock()
	if s.pending && s.m.T == "r" && m.T == "r" {
		// Sum deltas so no motion is lost; the newest modifier state wins.
		s.m.DX += m.DX
		s.m.DY += m.DY
		s.m.F = m.F
	} else {
		s.m = m // absolute position (or a mode switch): the latest simply wins
	}
	s.pending = true
	s.mu.Unlock()
	select {
	case w <- struct{}{}:
	default: // a wake is already queued; the pump will see the merged move
	}
}

// moveSeq turns sequenced moves from the unordered, never-retransmitted move channel
// back into a correct stream. Packets there can be lost or arrive out of order, so:
//   - every "m"/"c" carries a sequence number N; anything not newer than the last
//     accepted one is dropped — a late old position must never yank the cursor back;
//   - relative motion travels as running totals ("c": CX/CY since the channel opened)
//     and is differenced here, so a lost packet's motion arrives with the next one
//     instead of vanishing (summed deltas would make a lost packet's turn permanent).
//
// One per session: the browser restarts N and the totals on every new move channel.
// The browser also re-sends its latest state on the reliable channel when motion
// stops and before a click, which fixes the last-packet-lost case; both channels
// feed this one sequence.
type moveSeq struct {
	mu     sync.Mutex
	n      int64
	cx, cy float64
}

// accept returns the move to inject ("m", or "c" rendered as an "r" delta) or false
// if it is stale. Unsequenced (N == 0) "m"/"r" pass through unchanged — that is the
// reliable-channel path used when no move channel exists.
func (q *moveSeq) accept(m clientInput) (clientInput, bool) {
	if m.N == 0 {
		return m, m.T == "m" || m.T == "r"
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if m.N <= q.n {
		return m, false
	}
	q.n = m.N
	switch m.T {
	case "m":
		return m, true
	case "c":
		dx, dy := m.CX-q.cx, m.CY-q.cy
		q.cx, q.cy = m.CX, m.CY
		if dx == 0 && dy == 0 {
			return m, false
		}
		return clientInput{T: "r", DX: dx, DY: dy, F: m.F}, true
	}
	return m, false
}

// take returns the pending move as a helper line and clears the slot, or nil.
//
// Summed deltas can outgrow the per-line clamp (±maxDelta) when the injector stalls
// mid-flick; rendering that as one clamped line would silently drop the excess. So an
// oversized delta leaves in maxDelta-sized pieces: take returns one piece and keeps
// the rest pending. Callers loop until take returns nil.
func (s *moveSlot) take() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pending {
		return nil
	}
	if s.m.T == "r" && (math.Abs(s.m.DX) > maxDelta || math.Abs(s.m.DY) > maxDelta) {
		piece := s.m
		piece.DX, piece.DY = clampDelta(s.m.DX), clampDelta(s.m.DY)
		s.m.DX -= piece.DX
		s.m.DY -= piece.DY
		return piece.line()
	}
	s.pending = false
	return s.m.line()
}

// clear drops any pending move.
func (s *moveSlot) clear() {
	s.mu.Lock()
	s.pending = false
	s.mu.Unlock()
}
