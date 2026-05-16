//go:build vector && arm64

package vector

import "math"

func cosineDistance(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 1
	}
	dot, normA, normB := cosineKernelNEON(&a[0], &b[0], len(a))
	denom := float32(math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
	if denom == 0 {
		return 1
	}
	return 1 - dot/denom
}

func l2Distance(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	return l2KernelNEON(&a[0], &b[0], len(a))
}

func innerProductDistance(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	return -dotKernelNEON(&a[0], &b[0], len(a))
}

//go:noescape
func l2KernelNEON(a, b *float32, n int) float32

//go:noescape
func dotKernelNEON(a, b *float32, n int) float32

//go:noescape
func cosineKernelNEON(a, b *float32, n int) (dot, normA, normB float32)
