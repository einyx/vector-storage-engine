//go:build vector

package vector

import (
	"math"
	mrand "math/rand"
	"testing"
)

func oracleCosine(a, b []float32) float32 {
	var dot, na, nb float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		na += ai * ai
		nb += bi * bi
	}
	denom := math.Sqrt(na) * math.Sqrt(nb)
	if denom == 0 {
		return 1
	}
	return float32(1 - dot/denom)
}

func oracleL2(a, b []float32) float32 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return float32(s)
}

func oracleIP(a, b []float32) float32 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return float32(-s)
}

func TestDistance_FuzzAgainstOracle(t *testing.T) {
	rng := mrand.New(mrand.NewSource(1))
	dims := []int{1, 3, 4, 7, 8, 15, 16, 17, 64, 128, 384, 768, 1024}
	for _, dim := range dims {
		a := make([]float32, dim)
		b := make([]float32, dim)
		for i := range a {
			a[i] = rng.Float32()*2 - 1
			b[i] = rng.Float32()*2 - 1
		}
		// L2 identity has bounded relative error; pick a tolerance scaled
		// to the magnitudes involved.
		tolL2 := float32(math.Max(1e-4, float64(oracleL2(a, b))*1e-4))
		if got, want := L2Distance(a, b), oracleL2(a, b); math.Abs(float64(got-want)) > float64(tolL2) {
			t.Errorf("dim=%d L2: got %v want %v (tol %v)", dim, got, want, tolL2)
		}
		if got, want := CosineDistance(a, b), oracleCosine(a, b); math.Abs(float64(got-want)) > 1e-5 {
			t.Errorf("dim=%d Cosine: got %v want %v", dim, got, want)
		}
		if got, want := InnerProductDistance(a, b), oracleIP(a, b); math.Abs(float64(got-want)) > 1e-4 {
			t.Errorf("dim=%d IP: got %v want %v", dim, got, want)
		}
	}
}

func TestCosineDistance_IdenticalVectors(t *testing.T) {
	a := []float32{1, 2, 3}
	d := CosineDistance(a, a)
	if d > 1e-6 {
		t.Fatalf("identical vectors should have distance ~0, got %f", d)
	}
}

func TestCosineDistance_OrthogonalVectors(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	d := CosineDistance(a, b)
	if math.Abs(float64(d)-1.0) > 1e-6 {
		t.Fatalf("orthogonal vectors should have distance ~1, got %f", d)
	}
}

func TestCosineDistance_OppositeVectors(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{-1, 0}
	d := CosineDistance(a, b)
	if math.Abs(float64(d)-2.0) > 1e-6 {
		t.Fatalf("opposite vectors should have distance ~2, got %f", d)
	}
}

func TestL2Distance(t *testing.T) {
	a := []float32{0, 0, 0}
	b := []float32{3, 4, 0}
	d := L2Distance(a, b)
	if math.Abs(float64(d)-25.0) > 1e-6 {
		t.Fatalf("expected squared L2 distance 25, got %f", d)
	}
}

func TestInnerProductDistance(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{4, 5, 6}
	d := InnerProductDistance(a, b)
	if math.Abs(float64(d)-(-32.0)) > 1e-6 {
		t.Fatalf("expected inner product distance -32, got %f", d)
	}
}

func TestDistanceFuncFor(t *testing.T) {
	tests := []struct {
		metric DistanceMetric
		wantOk bool
	}{
		{Cosine, true},
		{L2, true},
		{InnerProduct, true},
		{DistanceMetric("unknown"), false},
	}
	for _, tt := range tests {
		fn, err := DistanceFuncFor(tt.metric)
		if tt.wantOk && err != nil {
			t.Fatalf("expected ok for %s, got error: %v", tt.metric, err)
		}
		if !tt.wantOk && err == nil {
			t.Fatalf("expected error for %s, got fn: %v", tt.metric, fn)
		}
	}
}
