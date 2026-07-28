package host

import "testing"

// translateInput is the trust boundary between the browser and CGEvent injection —
// every accepted message becomes a line on the helper's stdin, so these cases pin
// both the happy-path rendering and the rejection of malformed/out-of-range input.
func TestTranslateInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // empty means "must be rejected"
	}{
		{"abs move", `{"t":"m","x":0.5,"y":0.25,"f":0}`, "m 0.50000 0.25000 0\n"},
		{"abs move out of unit", `{"t":"m","x":1.5,"y":0.5,"f":0}`, ""},
		{"rel move", `{"t":"r","dx":12.34,"dy":-4,"f":8}`, "r 12.3 -4.0 8\n"},
		{"rel move clamped", `{"t":"r","dx":99999,"dy":-99999,"f":0}`, "r 1000.0 -1000.0 0\n"},
		{"down with coords", `{"t":"d","b":0,"x":0.1,"y":0.9,"f":1}`, "d 0 0.10000 0.90000 1\n"},
		{"down bad button", `{"t":"d","b":3,"x":0.1,"y":0.9,"f":0}`, ""},
		{"down coordless", `{"t":"D","b":2,"f":0}`, "D 2 0\n"},
		{"down coordless bad button", `{"t":"D","b":-1,"f":0}`, ""},
		{"up", `{"t":"u","b":1,"f":0}`, "u 1 0\n"},
		{"scroll clamped", `{"t":"w","dx":500,"dy":-500,"f":0}`, "w 120.0 -120.0 0\n"},
		{"key down", `{"t":"k","c":"Escape","f":0}`, "k Escape 0\n"},
		{"key up", `{"t":"K","c":"ArrowLeft","f":2}`, "K ArrowLeft 2\n"},
		{"key with space rejected", `{"t":"k","c":"bad key","f":0}`, ""},
		{"key with newline rejected", `{"t":"k","c":"a\nb","f":0}`, ""},
		{"release all", `{"t":"x"}`, "x\n"},
		{"bad modifier mask", `{"t":"m","x":0.5,"y":0.5,"f":16}`, ""},
		{"unknown type", `{"t":"zz"}`, ""},
		{"not json", `garbage`, ""},
		{"oversized", `{"t":"k","c":"KeyA","f":0,"pad":"` + string(make([]byte, 300)) + `"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := translateInput([]byte(tc.in))
			if tc.want == "" {
				if ok {
					t.Fatalf("expected rejection, got %q", got)
				}
				return
			}
			if !ok {
				t.Fatalf("expected %q, got rejection", tc.want)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
