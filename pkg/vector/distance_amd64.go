//go:build vector && amd64

package vector

import (
	"math"

	"golang.org/x/sys/cpu"
)

func cosineDistance(a, b []float32) float32 {
	if cpu.X86.HasAVX && cpu.X86.HasFMA {
		if len(a) != len(b) || len(a) == 0 {
			return 1
		}
		dot, normA, normB := cosineKernelAVX(&a[0], &b[0], len(a))
		denom := float32(math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
		if denom == 0 {
			return 1
		}
		return 1 - dot/denom
	}
	return cosineDistanceScalar(a, b)
}

func l2Distance(a, b []float32) float32 {
	if cpu.X86.HasAVX && cpu.X86.HasFMA {
		if len(a) != len(b) || len(a) == 0 {
			return 0
		}
		return l2KernelAVX(&a[0], &b[0], len(a))
	}
	return l2DistanceScalar(a, b)
}

func innerProductDistance(a, b []float32) float32 {
	if cpu.X86.HasAVX && cpu.X86.HasFMA {
		if len(a) != len(b) || len(a) == 0 {
			return 0
		}
		return -dotKernelAVX(&a[0], &b[0], len(a))
	}
	return innerProductDistanceScalar(a, b)
}

//go:noescape
func l2KernelAVX(a, b *float32, n int) float32

//go:noescape
func dotKernelAVX(a, b *float32, n int) float32

//go:noescape
func cosineKernelAVX(a, b *float32, n int) (dot, normA, normB float32)
