//go:build vector

package vector

import "fmt"

func CosineDistance(a, b []float32) float32      { return cosineDistance(a, b) }
func L2Distance(a, b []float32) float32           { return l2Distance(a, b) }
func InnerProductDistance(a, b []float32) float32 { return innerProductDistance(a, b) }

func DistanceFuncFor(metric DistanceMetric) (DistanceFunc, error) {
	switch metric {
	case Cosine:
		return CosineDistance, nil
	case L2:
		return L2Distance, nil
	case InnerProduct:
		return InnerProductDistance, nil
	default:
		return nil, fmt.Errorf("unsupported distance metric: %s", metric)
	}
}
