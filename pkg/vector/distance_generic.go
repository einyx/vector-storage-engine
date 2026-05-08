//go:build vector && !arm64

package vector

import "math"

func cosineDistance(a, b []float32) float32 {
	dot, normA, normB := dotAndNorms(a, b)
	denom := float32(math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
	if denom == 0 {
		return 1
	}
	return 1 - dot/denom
}

func l2Distance(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	n := len(a)
	var s0, s1, s2, s3 float32
	i := 0
	for ; i <= n-4; i += 4 {
		ax := a[i : i+4 : i+4]
		bx := b[i : i+4 : i+4]
		d0 := ax[0] - bx[0]
		d1 := ax[1] - bx[1]
		d2 := ax[2] - bx[2]
		d3 := ax[3] - bx[3]
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	sum := (s0 + s1) + (s2 + s3)
	for ; i < n; i++ {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

func innerProductDistance(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	n := len(a)
	var s0, s1, s2, s3 float32
	i := 0
	for ; i <= n-4; i += 4 {
		ax := a[i : i+4 : i+4]
		bx := b[i : i+4 : i+4]
		s0 += ax[0] * bx[0]
		s1 += ax[1] * bx[1]
		s2 += ax[2] * bx[2]
		s3 += ax[3] * bx[3]
	}
	dot := (s0 + s1) + (s2 + s3)
	for ; i < n; i++ {
		dot += a[i] * b[i]
	}
	return -dot
}

func dotAndNorms(a, b []float32) (dot, normA, normB float32) {
	if len(a) != len(b) {
		return 0, 0, 0
	}
	n := len(a)
	var d0, d1, d2, d3 float32
	var na0, na1, na2, na3 float32
	var nb0, nb1, nb2, nb3 float32
	i := 0
	for ; i <= n-4; i += 4 {
		ax := a[i : i+4 : i+4]
		bx := b[i : i+4 : i+4]
		d0 += ax[0] * bx[0]
		d1 += ax[1] * bx[1]
		d2 += ax[2] * bx[2]
		d3 += ax[3] * bx[3]
		na0 += ax[0] * ax[0]
		na1 += ax[1] * ax[1]
		na2 += ax[2] * ax[2]
		na3 += ax[3] * ax[3]
		nb0 += bx[0] * bx[0]
		nb1 += bx[1] * bx[1]
		nb2 += bx[2] * bx[2]
		nb3 += bx[3] * bx[3]
	}
	dot = (d0 + d1) + (d2 + d3)
	normA = (na0 + na1) + (na2 + na3)
	normB = (nb0 + nb1) + (nb2 + nb3)
	for ; i < n; i++ {
		av, bv := a[i], b[i]
		dot += av * bv
		normA += av * av
		normB += bv * bv
	}
	return
}
