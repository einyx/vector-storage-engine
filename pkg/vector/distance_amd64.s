//go:build vector && amd64

#include "textflag.h"

// Horizontal reduction of four float32 lanes in XMM reg (AVX/VEX xmm ops).
#define HSUM_XMM(R) \
	VHADDPS R, R, R \
	VHADDPS R, R, R

// func dotKernelAVX(a, b *float32, n int) float32
TEXT ·dotKernelAVX(SB), NOSPLIT, $0-32
	MOVQ	a+0(FP), AX
	MOVQ	b+8(FP), BX
	MOVQ	n+16(FP), CX

	VXORPS	X0, X0, X0

	MOVQ	CX, DX
	SHRQ	$2, DX
	JZ	dot_tail

dot_loop4:
	VMOVUPS	(AX), X1
	VMOVUPS	(BX), X2
	VFMADD231PS	X2, X1, X0
	ADDQ	$16, AX
	ADDQ	$16, BX
	SUBQ	$1, DX
	JNZ	dot_loop4

dot_tail:
	HSUM_XMM(X0)

	ANDQ	$3, CX
	JZ	dot_done

dot_scalar:
	MOVSS	(AX), X1
	MOVSS	(BX), X2
	VFMADD231SS	X2, X1, X0
	ADDQ	$4, AX
	ADDQ	$4, BX
	SUBQ	$1, CX
	JNZ	dot_scalar

dot_done:
	VZEROUPPER
	MOVSS	X0, ret+24(FP)
	RET

// func cosineKernelAVX(a, b *float32, n int) (dot, normA, normB float32)
TEXT ·cosineKernelAVX(SB), NOSPLIT, $0-40
	MOVQ	a+0(FP), AX
	MOVQ	b+8(FP), BX
	MOVQ	n+16(FP), CX

	VXORPS	X0, X0, X0	// dot
	VXORPS	X4, X4, X4	// normA
	VXORPS	X5, X5, X5	// normB

	MOVQ	CX, DX
	SHRQ	$2, DX
	JZ	cos_tail

cos_loop4:
	VMOVUPS	(AX), X1
	VMOVUPS	(BX), X2
	VFMADD231PS	X2, X1, X0
	VFMADD231PS	X1, X1, X4
	VFMADD231PS	X2, X2, X5
	ADDQ	$16, AX
	ADDQ	$16, BX
	SUBQ	$1, DX
	JNZ	cos_loop4

cos_tail:
	HSUM_XMM(X0)
	HSUM_XMM(X4)
	HSUM_XMM(X5)

	ANDQ	$3, CX
	JZ	cos_done

cos_scalar:
	MOVSS	(AX), X1
	MOVSS	(BX), X2
	VFMADD231SS	X2, X1, X0
	VFMADD231SS	X1, X1, X4
	VFMADD231SS	X2, X2, X5
	ADDQ	$4, AX
	ADDQ	$4, BX
	SUBQ	$1, CX
	JNZ	cos_scalar

cos_done:
	VZEROUPPER
	MOVSS	X0, dot+24(FP)
	MOVSS	X4, normA+28(FP)
	MOVSS	X5, normB+32(FP)
	RET

// func l2KernelAVX(a, b *float32, n int) float32
// ||a-b||^2 = ||a||^2 + ||b||^2 - 2(a·b) (same trick as ARM NEON).
TEXT ·l2KernelAVX(SB), NOSPLIT, $0-32
	MOVQ	a+0(FP), AX
	MOVQ	b+8(FP), BX
	MOVQ	n+16(FP), CX

	VXORPS	X0, X0, X0
	VXORPS	X4, X4, X4
	VXORPS	X5, X5, X5

	MOVQ	CX, DX
	SHRQ	$2, DX
	JZ	l2_tail

l2_loop4:
	VMOVUPS	(AX), X1
	VMOVUPS	(BX), X2
	VFMADD231PS	X2, X1, X0
	VFMADD231PS	X1, X1, X4
	VFMADD231PS	X2, X2, X5
	ADDQ	$16, AX
	ADDQ	$16, BX
	SUBQ	$1, DX
	JNZ	l2_loop4

l2_tail:
	HSUM_XMM(X0)
	HSUM_XMM(X4)
	HSUM_XMM(X5)

	ANDQ	$3, CX
	JZ	l2_reduce

l2_scalar:
	MOVSS	(AX), X1
	MOVSS	(BX), X2
	VFMADD231SS	X2, X1, X0
	VFMADD231SS	X1, X1, X4
	VFMADD231SS	X2, X2, X5
	ADDQ	$4, AX
	ADDQ	$4, BX
	SUBQ	$1, CX
	JNZ	l2_scalar

l2_reduce:
	// F4 = normA + normB - 2*dot
	VADDPS	X5, X4, X4
	VADDPS	X0, X0, X0
	VSUBPS	X0, X4, X4
	VZEROUPPER
	MOVSS	X4, ret+24(FP)
	RET
