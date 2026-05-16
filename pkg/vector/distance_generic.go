//go:build vector && !arm64 && !amd64

package vector

func cosineDistance(a, b []float32) float32 {
	return cosineDistanceScalar(a, b)
}

func l2Distance(a, b []float32) float32 {
	return l2DistanceScalar(a, b)
}

func innerProductDistance(a, b []float32) float32 {
	return innerProductDistanceScalar(a, b)
}
