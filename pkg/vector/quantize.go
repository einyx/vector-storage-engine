//go:build vector

package vector

import (
	"encoding/binary"
	"fmt"
	"math"
)

// quantParams is the per-dimension affine map used by int8 scalar quantization.
// dequant(q) ≈ float32(q+128) * scale[d] + min[d]
type quantParams struct {
	min   []float32
	scale []float32
}

// computeQuantParams scans points to build per-dimension min/scale.
// scale = (max-min)/255 with a safe-zero fallback so dims with no variance
// don't divide-by-zero.
func computeQuantParams(points []Point, dim int) quantParams {
	mins := make([]float32, dim)
	maxs := make([]float32, dim)
	for d := 0; d < dim; d++ {
		mins[d] = math.MaxFloat32
		maxs[d] = -math.MaxFloat32
	}
	for _, p := range points {
		for d, v := range p.Vector {
			if v < mins[d] {
				mins[d] = v
			}
			if v > maxs[d] {
				maxs[d] = v
			}
		}
	}
	scales := make([]float32, dim)
	for d := 0; d < dim; d++ {
		r := maxs[d] - mins[d]
		if r <= 0 {
			scales[d] = 1
		} else {
			scales[d] = r / 255
		}
	}
	return quantParams{min: mins, scale: scales}
}

func quantizeOne(v []float32, qp quantParams) []int8 {
	out := make([]int8, len(v))
	for d, x := range v {
		q := math.Round(float64((x - qp.min[d]) / qp.scale[d]))
		// signed range maps [0,255] → [-128,127]
		q -= 128
		if q < -128 {
			q = -128
		} else if q > 127 {
			q = 127
		}
		out[d] = int8(q)
	}
	return out
}

func dequantizeOne(q []int8, qp quantParams) []float32 {
	out := make([]float32, len(q))
	for d, b := range q {
		out[d] = float32(int(b)+128)*qp.scale[d] + qp.min[d]
	}
	return out
}

// encodeQuantizedVectors writes the vectors_q.bin payload:
//
//	u32 count, u32 dim,
//	dim*f32 min,
//	dim*f32 scale,
//	N * (u64 id + dim*int8 quantized vector)
func encodeQuantizedVectors(points []Point, qp quantParams, dim int) []byte {
	// Header + per-dim params + per-point payload.
	headerLen := 8 + 4*dim*2
	pointLen := 8 + dim
	out := make([]byte, 0, headerLen+pointLen*len(points))

	out = binary.LittleEndian.AppendUint32(out, uint32(len(points)))
	out = binary.LittleEndian.AppendUint32(out, uint32(dim))
	for _, m := range qp.min {
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(m))
	}
	for _, s := range qp.scale {
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(s))
	}
	for _, p := range points {
		out = binary.LittleEndian.AppendUint64(out, p.ID)
		q := quantizeOne(p.Vector, qp)
		for _, b := range q {
			out = append(out, byte(b))
		}
	}
	return out
}

// decodeQuantizedVectors reverses encodeQuantizedVectors and dequantizes.
func decodeQuantizedVectors(data []byte, expectedDim int) ([]Point, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("quantized vectors data too short")
	}
	count := int(binary.LittleEndian.Uint32(data[0:4]))
	dim := int(binary.LittleEndian.Uint32(data[4:8]))
	if dim != expectedDim {
		return nil, fmt.Errorf("dimension mismatch: expected %d, got %d", expectedDim, dim)
	}
	off := 8
	if len(data) < off+4*dim*2 {
		return nil, fmt.Errorf("truncated quant params")
	}
	mins := make([]float32, dim)
	for d := 0; d < dim; d++ {
		mins[d] = math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))
		off += 4
	}
	scales := make([]float32, dim)
	for d := 0; d < dim; d++ {
		scales[d] = math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))
		off += 4
	}
	qp := quantParams{min: mins, scale: scales}

	pointLen := 8 + dim
	if len(data)-off < pointLen*count {
		return nil, fmt.Errorf("truncated quantized points")
	}
	points := make([]Point, 0, count)
	for i := 0; i < count; i++ {
		id := binary.LittleEndian.Uint64(data[off:])
		off += 8
		q := make([]int8, dim)
		for d := 0; d < dim; d++ {
			q[d] = int8(data[off])
			off++
		}
		points = append(points, Point{ID: id, Vector: dequantizeOne(q, qp)})
	}
	return points, nil
}
