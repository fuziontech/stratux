package common

import (
	"math"
	"testing"
)

func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestAngleNormalization(t *testing.T) {
	// Verify O(1) handling of NaN, ±Inf, and huge floats without hanging.
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if !math.IsNaN(RadiansRel(bad)) {
			t.Errorf("RadiansRel(%v) = %v, want NaN", bad, RadiansRel(bad))
		}
		if !math.IsNaN(DegreesRel(bad)) {
			t.Errorf("DegreesRel(%v) = %v, want NaN", bad, DegreesRel(bad))
		}
		if !math.IsNaN(DegreesHdg(bad)) {
			t.Errorf("DegreesHdg(%v) = %v, want NaN", bad, DegreesHdg(bad))
		}
	}

	// Huge finite angles must terminate immediately and stay in range.
	if r := RadiansRel(1e20); math.IsNaN(r) || r < -math.Pi || r > math.Pi {
		t.Errorf("RadiansRel(1e20) = %v, out of [-Pi, Pi]", r)
	}
	if d := DegreesRel(1e20); math.IsNaN(d) || d < -180 || d > 180 {
		t.Errorf("DegreesRel(1e20) = %v, out of [-180, 180]", d)
	}
	if h := DegreesHdg(1e20); math.IsNaN(h) || h < 0 || h >= 360 {
		t.Errorf("DegreesHdg(1e20) = %v, out of [0, 360)", h)
	}

	radCases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{180, math.Pi},
		{-180, -math.Pi},
		{270, -math.Pi / 2},
		{-270, math.Pi / 2},
		{540, math.Pi},
		{-540, -math.Pi},
		{720, 0},
	}
	for _, tc := range radCases {
		if got := RadiansRel(tc.in); !almostEqual(got, tc.want, 1e-12) {
			t.Errorf("RadiansRel(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}

	degRelCases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{math.Pi, 180},
		{-math.Pi, -180},
		{1.5 * math.Pi, -90},
		{-1.5 * math.Pi, 90},
		{3 * math.Pi, 180},
		{-3 * math.Pi, -180},
	}
	for _, tc := range degRelCases {
		if got := DegreesRel(tc.in); !almostEqual(got, tc.want, 1e-12) {
			t.Errorf("DegreesRel(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}

	hdgCases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{math.Pi / 2, 90},
		{math.Pi, 180},
		{-math.Pi / 2, 270},
		{2 * math.Pi, 0},
		{3 * math.Pi, 180},
		{5 * math.Pi / 2, 90},
		{-1e-20, 0},
	}
	for _, tc := range hdgCases {
		got := DegreesHdg(tc.in)
		if got < 0 || got >= 360 {
			t.Errorf("DegreesHdg(%v) = %v, out of [0, 360)", tc.in, got)
		}
		if !almostEqual(got, tc.want, 1e-12) {
			t.Errorf("DegreesHdg(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDistance(t *testing.T) {
	// Identical points must return 0 (never NaN).
	coords := [][2]float64{
		{0, 0},
		{37.7749, -122.4194},
		{51.5074, -0.1278},
		{-33.8688, 151.2093},
		{89.9999, 179.9999},
	}
	for _, c := range coords {
		dist, brg := Distance(c[0], c[1], c[0], c[1])
		if math.IsNaN(dist) || dist != 0 {
			t.Errorf("Distance(%v,%v -> same) = %v, want 0", c[0], c[1], dist)
		}
		if math.IsNaN(brg) {
			t.Errorf("Distance(%v,%v -> same) bearing = NaN", c[0], c[1])
		}
	}

	// 1 degree north along meridian ≈ 111,195 m, bearing 0°.
	dist, brg := Distance(0, 0, 1, 0)
	if !almostEqual(dist, 111195.0, 20.0) {
		t.Errorf("Distance(0,0 -> 1,0) = %v, want ~111195", dist)
	}
	if !almostEqual(brg, 0, 1e-9) {
		t.Errorf("bearing(0,0 -> 1,0) = %v, want 0", brg)
	}

	// 1 degree east along equator ≈ 111,195 m, bearing 90°.
	dist, brg = Distance(0, 0, 0, 1)
	if !almostEqual(dist, 111195.0, 20.0) {
		t.Errorf("Distance(0,0 -> 0,1) = %v, want ~111195", dist)
	}
	if !almostEqual(brg, 90, 1e-9) {
		t.Errorf("bearing(0,0 -> 0,1) = %v, want 90", brg)
	}

	// Antipodal points must not produce NaN.
	dist, brg = Distance(0, 0, 0, 180)
	if math.IsNaN(dist) || !almostEqual(dist, math.Pi*6371008.8, 1.0) {
		t.Errorf("antipodal Distance = %v, want ~%v", dist, math.Pi*6371008.8)
	}
	if math.IsNaN(brg) {
		t.Errorf("antipodal bearing = NaN")
	}
}

func TestLinReg(t *testing.T) {
	// Standard line y = 2x + 3.
	s, b, ok := LinReg([]float64{1, 2, 3, 4}, []float64{5, 7, 9, 11})
	if !ok || !almostEqual(s, 2, 1e-12) || !almostEqual(b, 3, 1e-12) {
		t.Fatalf("LinReg linear = (%v, %v, %v), want (2, 3, true)", s, b, ok)
	}

	// Large-offset x values where single-pass Sxx - Sx*Sx/n suffers catastrophic cancellation.
	base := 1e12
	s, b, ok = LinReg([]float64{base, base + 1, base + 2}, []float64{10, 13, 16})
	if !ok || !almostEqual(s, 3, 1e-9) {
		t.Fatalf("LinReg large offset = (%v, %v, %v), want slope 3, valid true", s, b, ok)
	}

	// Identical large x values must be detected as degenerate (invalid).
	if _, _, ok := LinReg([]float64{base, base, base}, []float64{1, 2, 3}); ok {
		t.Errorf("LinReg identical x should be invalid")
	}

	// Mismatched lengths, too short, NaN, Inf.
	if _, _, ok := LinReg([]float64{1, 2}, []float64{1}); ok {
		t.Errorf("LinReg mismatched lengths should be invalid")
	}
	if _, _, ok := LinReg([]float64{1}, []float64{1}); ok {
		t.Errorf("LinReg n<2 should be invalid")
	}
	if _, _, ok := LinReg([]float64{1, math.NaN()}, []float64{1, 2}); ok {
		t.Errorf("LinReg NaN input should be invalid")
	}
	if _, _, ok := LinReg([]float64{1, math.Inf(1)}, []float64{1, 2}); ok {
		t.Errorf("LinReg Inf input should be invalid")
	}
}

func TestLinRegWeighted(t *testing.T) {
	// Exact weighted fit y = -0.5x + 10.
	x := []float64{0, 2, 4, 6}
	y := []float64{10, 9, 8, 7}
	w := []float64{1, 2, 2, 1}
	s, b, ok := LinRegWeighted(x, y, w)
	if !ok || !almostEqual(s, -0.5, 1e-12) || !almostEqual(b, 10, 1e-12) {
		t.Fatalf("LinRegWeighted = (%v, %v, %v), want (-0.5, 10, true)", s, b, ok)
	}

	// Large-offset x values.
	base := 1e12
	s, _, ok = LinRegWeighted([]float64{base, base + 1, base + 2}, []float64{4, 6, 8}, []float64{0.5, 1, 0.5})
	if !ok || !almostEqual(s, 2, 1e-9) {
		t.Fatalf("LinRegWeighted large offset slope = %v (ok=%v), want 2", s, ok)
	}

	// Zero sum of weights, single non-zero weight, NaN/Inf.
	if _, _, ok := LinRegWeighted(x, y, []float64{0, 0, 0, 0}); ok {
		t.Errorf("LinRegWeighted zero weights should be invalid")
	}
	if _, _, ok := LinRegWeighted(x, y, []float64{0, 1, 0, 0}); ok {
		t.Errorf("LinRegWeighted single non-zero weight should be invalid")
	}
	if _, _, ok := LinRegWeighted([]float64{1, 2}, []float64{1, math.NaN()}, []float64{1, 1}); ok {
		t.Errorf("LinRegWeighted NaN should be invalid")
	}
	if _, _, ok := LinRegWeighted([]float64{1, 2}, []float64{1, 2}, []float64{1}); ok {
		t.Errorf("LinRegWeighted mismatched lengths should be invalid")
	}
}

func TestRoundToInt16(t *testing.T) {
	cases := []struct {
		in   float64
		want int16
	}{
		{0, 0},
		{1.4, 1},
		{1.5, 2},
		{-1.4, -1},
		{-1.5, -2},
		{math.NaN(), 0},
		{math.Inf(1), math.MaxInt16},
		{math.Inf(-1), math.MinInt16},
		{100000, math.MaxInt16},
		{-100000, math.MinInt16},
		{32767.0, math.MaxInt16},
		{-32768.0, math.MinInt16},
	}
	for _, tc := range cases {
		if got := RoundToInt16(tc.in); got != tc.want {
			t.Errorf("RoundToInt16(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTriCubeWeight(t *testing.T) {
	if got := TriCubeWeight(10, 5, 10); !almostEqual(got, 1, 1e-12) {
		t.Errorf("TriCubeWeight at center = %v, want 1", got)
	}
	if got := TriCubeWeight(10, 5, 15); got != 0 {
		t.Errorf("TriCubeWeight at edge = %v, want 0", got)
	}
	if got := TriCubeWeight(10, 0, 10); got != 0 {
		t.Errorf("TriCubeWeight(halfwidth=0) = %v, want 0", got)
	}
	if got := TriCubeWeight(10, -5, 10); got != 0 {
		t.Errorf("TriCubeWeight(halfwidth<0) = %v, want 0", got)
	}
	if got := TriCubeWeight(10, math.NaN(), 10); got != 0 {
		t.Errorf("TriCubeWeight(halfwidth=NaN) = %v, want 0", got)
	}
}

func TestCalcAltitude(t *testing.T) {
	if got := CalcAltitude(1013.25, 0); !almostEqual(got, 0, 1e-6) {
		t.Errorf("CalcAltitude(1013.25, 0) = %v, want 0", got)
	}
	if got := CalcAltitude(1013.25, 250); !almostEqual(got, 250, 1e-6) {
		t.Errorf("CalcAltitude(1013.25, 250) = %v, want 250", got)
	}
	for _, bad := range []float64{0, -100, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := CalcAltitude(bad, 100); got != 0 || math.IsNaN(got) {
			t.Errorf("CalcAltitude(%v, 100) = %v, want 0", bad, got)
		}
	}
}

func TestArrayStats(t *testing.T) {
	if _, ok := ArrayMin(nil); ok {
		t.Errorf("ArrayMin(nil) should be invalid")
	}
	if _, ok := ArrayMax(nil); ok {
		t.Errorf("ArrayMax(nil) should be invalid")
	}
	if _, ok := ArrayRange(nil); ok {
		t.Errorf("ArrayRange(nil) should be invalid")
	}
	if _, ok := Mean(nil); ok {
		t.Errorf("Mean(nil) should be invalid")
	}
	if _, ok := Stdev([]float64{1}); ok {
		t.Errorf("Stdev(len=1) should be invalid")
	}

	vals := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	min, ok := ArrayMin(vals)
	if !ok || min != 2 {
		t.Errorf("ArrayMin = (%v, %v), want (2, true)", min, ok)
	}
	max, ok := ArrayMax(vals)
	if !ok || max != 9 {
		t.Errorf("ArrayMax = (%v, %v), want (9, true)", max, ok)
	}
	rng, ok := ArrayRange(vals)
	if !ok || rng != 7 {
		t.Errorf("ArrayRange = (%v, %v), want (7, true)", rng, ok)
	}
	mean, ok := Mean(vals)
	if !ok || !almostEqual(mean, 5, 1e-12) {
		t.Errorf("Mean = (%v, %v), want (5, true)", mean, ok)
	}
	sd, ok := Stdev(vals)
	if !ok || !almostEqual(sd, math.Sqrt(32.0/7.0), 1e-12) {
		t.Errorf("Stdev = (%v, %v), want (%v, true)", sd, ok, math.Sqrt(32.0/7.0))
	}
}
