package host

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// baseTranslateInput is translateInput exactly as it was at 300908d, before the
// decodeInput/line() refactor. It is the oracle: the refactor must accept exactly
// the same messages and render exactly the same lines.
func baseTranslateInput(data []byte) ([]byte, bool) {
	if len(data) > 256 {
		return nil, false
	}
	var m clientInput
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	if m.F < 0 || m.F > 15 {
		return nil, false
	}
	switch m.T {
	case "m":
		if !inUnit(m.X) || !inUnit(m.Y) {
			return nil, false
		}
		return []byte(fmt.Sprintf("m %.5f %.5f %d\n", m.X, m.Y, m.F)), true
	case "r":
		return []byte(fmt.Sprintf("r %.1f %.1f %d\n", clampDelta(m.DX), clampDelta(m.DY), m.F)), true
	case "D":
		if m.B < 0 || m.B > 2 {
			return nil, false
		}
		return []byte(fmt.Sprintf("D %d %d\n", m.B, m.F)), true
	case "d":
		if m.B < 0 || m.B > 2 || !inUnit(m.X) || !inUnit(m.Y) {
			return nil, false
		}
		return []byte(fmt.Sprintf("d %d %.5f %.5f %d\n", m.B, m.X, m.Y, m.F)), true
	case "u":
		if m.B < 0 || m.B > 2 {
			return nil, false
		}
		return []byte(fmt.Sprintf("u %d %d\n", m.B, m.F)), true
	case "w":
		return []byte(fmt.Sprintf("w %.1f %.1f %d\n", clampScroll(m.DX), clampScroll(m.DY), m.F)), true
	case "k", "K":
		if !validKeyCode(m.C) {
			return nil, false
		}
		return []byte(fmt.Sprintf("%s %s %d\n", m.T, m.C, m.F)), true
	case "x":
		return []byte("x\n"), true
	}
	return nil, false
}

func reviewCorpus() []string {
	c := []string{
		``, `{}`, `null`, `[]`, `{"t":""}`, `{"t":"kf"}`, `{"t":"q"}`, `{"t":"M"}`,
		`{"t":"m","x":0,"y":0}`, `{"t":"m","x":1,"y":1,"f":15}`, `{"t":"m","x":1.0001,"y":0}`,
		`{"t":"m","x":-0.0001,"y":0}`, `{"t":"m","x":0.5}`, `{"t":"m","x":"0.5","y":0.5}`,
		`{"t":"m","x":0.5,"y":0.5,"f":16}`, `{"t":"m","x":0.5,"y":0.5,"f":-1}`,
		`{"t":"r","dx":5000,"dy":-5000,"f":3}`, `{"t":"r","dx":1e308,"dy":-1e308}`, `{"t":"r"}`,
		`{"t":"D","b":3}`, `{"t":"D","b":-1}`, `{"t":"D","b":2,"f":8}`,
		`{"t":"d","b":0,"x":0.5,"y":2}`, `{"t":"d","b":1,"x":0.5,"y":0.5}`, `{"t":"d","b":5,"x":0.5,"y":0.5}`,
		`{"t":"u","b":0}`, `{"t":"u","b":9}`,
		`{"t":"w","dx":500,"dy":-500}`, `{"t":"w","dx":1,"dy":2,"f":20}`,
		`{"t":"k","c":"KeyA","f":1}`, `{"t":"K","c":"Key A"}`, `{"t":"k","c":"KeyA\n"}`,
		`{"t":"k","c":""}`, `{"t":"k","c":"ABCDEFGHIJKLMNOPQRSTU"}`, `{"t":"k","c":"ABCDEFGHIJKLMNOPQRST"}`,
		`{"t":"k","c":"Ä"}`, `{"t":"k","c":"x;rm"}`, `{"t":"x","f":15}`, `{"t":"x","f":99}`,
		`{"t":"m","x":0.5,"y":0.5,"b":1.5}`, `{"t":"m","x":0.5,"y":0.5,"f":1.5}`,
		`{"t":"m","x":0.5,"y":0.5}garbage`, `{"t":"m","t":"k","c":"KeyA"}`,
	}
	pad := strings.Repeat(" ", 256-len(`{"t":"x"}`))
	c = append(c, `{"t":"x"}`+pad[:len(pad)-1], `{"t":"x"}`+pad, `{"t":"x"}`+pad+" ")
	return c
}

// The refactor must be exactly as strict as the old translateInput.
func TestReviewTranslateInputParityWithBase(t *testing.T) {
	check := func(in string) {
		gotL, gotOK := translateInput([]byte(in))
		wantL, wantOK := baseTranslateInput([]byte(in))
		if gotOK != wantOK || string(gotL) != string(wantL) {
			t.Errorf("%q: got (%q,%v) want (%q,%v)", in, gotL, gotOK, wantL, wantOK)
		}
		// The move channel path (decodeInput + line) must never accept more either.
		m, ok := decodeInput([]byte(in))
		if ok != wantOK {
			t.Errorf("decodeInput %q: ok=%v, base ok=%v", in, ok, wantOK)
		}
		if ok && string(m.line()) != string(wantL) {
			t.Errorf("decodeInput %q: line %q, base %q", in, m.line(), wantL)
		}
	}
	for _, in := range reviewCorpus() {
		check(in)
	}
	rng := rand.New(rand.NewSource(1))
	types := []string{"m", "r", "d", "D", "u", "w", "k", "K", "x", "kf", "z"}
	codes := []string{"KeyA", "F11", "", "a b", "Arrow\nLeft", "Digit0"}
	for i := 0; i < 20000; i++ {
		in := fmt.Sprintf(`{"t":%q,"x":%g,"y":%g,"b":%d,"dx":%g,"dy":%g,"c":%q,"f":%d}`,
			types[rng.Intn(len(types))], rng.Float64()*1.4-0.2, rng.Float64()*1.4-0.2,
			rng.Intn(6)-2, rng.NormFloat64()*2000, rng.NormFloat64()*200,
			codes[rng.Intn(len(codes))], rng.Intn(20)-2)
		check(in)
	}
}

// Every line the move slot can ever emit must itself be a line translateInput would
// produce for some valid message: a sum of many clamped deltas stays clamped.
func TestReviewMoveSlotSumStaysClamped(t *testing.T) {
	var s moveSlot
	for i := 0; i < 50; i++ {
		s.put(clientInput{T: "r", DX: 900, DY: -900})
	}
	if got := string(s.take()); got != "r 1000.0 -1000.0 0\n" {
		t.Fatalf("summed deltas escaped the clamp: %q", got)
	}
}
