//go:build vector && arm64

#include "textflag.h"

// Horizontal-sum a 4-lane float32 vector into a scalar float.
// Lane extraction must complete before any scalar FADDS, because
// scalar FP writes zero the upper bits of the destination V register
// (the V register aliasing the F register).

// func dotKernelNEON(a, b *float32, n int) float32
TEXT ·dotKernelNEON(SB), NOSPLIT, $0-32
	MOVD	a+0(FP), R0
	MOVD	b+8(FP), R1
	MOVD	n+16(FP), R2

	VEOR	V0.B16, V0.B16, V0.B16

	LSR	$2, R2, R3
	CBZ	R3, dot_tail
dot_loop4:
	VLD1.P	16(R0), [V1.S4]
	VLD1.P	16(R1), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	SUB	$1, R3
	CBNZ	R3, dot_loop4

dot_tail:
	VMOV	V0.S[1], V16
	VMOV	V0.S[2], V17
	VMOV	V0.S[3], V18
	FADDS	F16, F0, F0
	FADDS	F18, F17, F17
	FADDS	F17, F0, F0

	AND	$3, R2, R3
	CBZ	R3, dot_done
dot_scalar:
	FMOVS	(R0), F1
	FMOVS	(R1), F2
	FMULS	F2, F1, F3
	FADDS	F3, F0, F0
	ADD	$4, R0
	ADD	$4, R1
	SUB	$1, R3
	CBNZ	R3, dot_scalar

dot_done:
	FMOVS	F0, ret+24(FP)
	RET

// func cosineKernelNEON(a, b *float32, n int) (dot, normA, normB float32)
// Computes dot(a,b), |a|^2, |b|^2 in a single fused pass.
TEXT ·cosineKernelNEON(SB), NOSPLIT, $0-40
	MOVD	a+0(FP), R0
	MOVD	b+8(FP), R1
	MOVD	n+16(FP), R2

	// V0 = dot, V4 = normA, V5 = normB
	VEOR	V0.B16, V0.B16, V0.B16
	VEOR	V4.B16, V4.B16, V4.B16
	VEOR	V5.B16, V5.B16, V5.B16

	LSR	$2, R2, R3
	CBZ	R3, cos_tail
cos_loop4:
	VLD1.P	16(R0), [V1.S4]
	VLD1.P	16(R1), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	VFMLA	V1.S4, V1.S4, V4.S4
	VFMLA	V2.S4, V2.S4, V5.S4
	SUB	$1, R3
	CBNZ	R3, cos_loop4

cos_tail:
	// Extract all lanes first, then reduce: scalar FP writes zero upper lanes.
	VMOV	V0.S[1], V16
	VMOV	V0.S[2], V17
	VMOV	V0.S[3], V18
	VMOV	V4.S[1], V19
	VMOV	V4.S[2], V20
	VMOV	V4.S[3], V21
	VMOV	V5.S[1], V22
	VMOV	V5.S[2], V23
	VMOV	V5.S[3], V24
	FADDS	F16, F0, F0
	FADDS	F18, F17, F17
	FADDS	F17, F0, F0
	FADDS	F19, F4, F4
	FADDS	F21, F20, F20
	FADDS	F20, F4, F4
	FADDS	F22, F5, F5
	FADDS	F24, F23, F23
	FADDS	F23, F5, F5

	AND	$3, R2, R3
	CBZ	R3, cos_done
cos_scalar:
	FMOVS	(R0), F1
	FMOVS	(R1), F2
	FMULS	F2, F1, F3
	FADDS	F3, F0, F0
	FMULS	F1, F1, F3
	FADDS	F3, F4, F4
	FMULS	F2, F2, F3
	FADDS	F3, F5, F5
	ADD	$4, R0
	ADD	$4, R1
	SUB	$1, R3
	CBNZ	R3, cos_scalar

cos_done:
	FMOVS	F0, dot+24(FP)
	FMOVS	F4, normA+28(FP)
	FMOVS	F5, normB+32(FP)
	RET

// func l2KernelNEON(a, b *float32, n int) float32
// Computes ||a-b||^2 via the identity ||a||^2 + ||b||^2 - 2(a·b).
// This avoids vector subtract (Go arm64 asm lacks VFSUB.S4).
// Trade-off: slight precision loss when a≈b; acceptable for ANN search.
TEXT ·l2KernelNEON(SB), NOSPLIT, $0-32
	MOVD	a+0(FP), R0
	MOVD	b+8(FP), R1
	MOVD	n+16(FP), R2

	VEOR	V0.B16, V0.B16, V0.B16
	VEOR	V4.B16, V4.B16, V4.B16
	VEOR	V5.B16, V5.B16, V5.B16

	LSR	$2, R2, R3
	CBZ	R3, l2_tail
l2_loop4:
	VLD1.P	16(R0), [V1.S4]
	VLD1.P	16(R1), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	VFMLA	V1.S4, V1.S4, V4.S4
	VFMLA	V2.S4, V2.S4, V5.S4
	SUB	$1, R3
	CBNZ	R3, l2_loop4

l2_tail:
	VMOV	V0.S[1], V16
	VMOV	V0.S[2], V17
	VMOV	V0.S[3], V18
	VMOV	V4.S[1], V19
	VMOV	V4.S[2], V20
	VMOV	V4.S[3], V21
	VMOV	V5.S[1], V22
	VMOV	V5.S[2], V23
	VMOV	V5.S[3], V24
	FADDS	F16, F0, F0
	FADDS	F18, F17, F17
	FADDS	F17, F0, F0
	FADDS	F19, F4, F4
	FADDS	F21, F20, F20
	FADDS	F20, F4, F4
	FADDS	F22, F5, F5
	FADDS	F24, F23, F23
	FADDS	F23, F5, F5

	AND	$3, R2, R3
	CBZ	R3, l2_done
l2_scalar:
	FMOVS	(R0), F1
	FMOVS	(R1), F2
	FMULS	F2, F1, F3
	FADDS	F3, F0, F0
	FMULS	F1, F1, F3
	FADDS	F3, F4, F4
	FMULS	F2, F2, F3
	FADDS	F3, F5, F5
	ADD	$4, R0
	ADD	$4, R1
	SUB	$1, R3
	CBNZ	R3, l2_scalar

l2_done:
	// result = normA + normB - 2*dot
	FADDS	F5, F4, F4
	FADDS	F0, F0, F0
	FSUBS	F0, F4, F4
	FMOVS	F4, ret+24(FP)
	RET
