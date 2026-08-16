package settlement

// 自实现 RIPEMD-160（纯 Go）。Go 1.26 已移除标准库 crypto/ripemd160；本实现用于 BTC 的
// HASH160 = RIPEMD160(SHA256(x))。算法严格遵循 RIPEMD-160 规范（双流水线各 80 步、5 轮；
// 左右两线使用不同布尔函数与逆序轮次、不同加性常数），输出为 5 个小端字（20 字节）。
// 实现与 golang.org/x/crypto/ripemd160 的轮次结构一致以保证正确性。

import "math/bits"

// RIPEMD-160 的消息字序（左线 _n / 右线 n_）与旋转量（左线 _r / 右线 r_），与规范一致。
var (
	rmdKL = []uint{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		7, 4, 13, 1, 10, 6, 15, 3, 12, 0, 9, 5, 2, 14, 11, 8,
		3, 10, 14, 4, 9, 15, 8, 1, 2, 7, 0, 6, 13, 11, 5, 12,
		1, 9, 11, 10, 0, 8, 12, 4, 13, 3, 7, 15, 14, 5, 6, 2,
		4, 0, 5, 9, 7, 12, 2, 10, 14, 1, 3, 8, 11, 6, 15, 13,
	}
	rmdKR = []uint{
		5, 14, 7, 0, 9, 2, 11, 4, 13, 6, 15, 8, 1, 10, 3, 12,
		6, 11, 3, 7, 0, 13, 5, 10, 14, 15, 8, 12, 4, 9, 1, 2,
		15, 5, 1, 3, 7, 14, 6, 9, 11, 8, 12, 2, 10, 0, 4, 13,
		8, 6, 4, 1, 3, 11, 15, 0, 5, 12, 2, 13, 9, 7, 10, 14,
		12, 15, 10, 4, 1, 5, 8, 7, 6, 2, 13, 14, 0, 3, 9, 11,
	}
	rmdSL = []uint{
		11, 14, 15, 12, 5, 8, 7, 9, 11, 13, 14, 15, 6, 7, 9, 8,
		7, 6, 8, 13, 11, 9, 7, 15, 7, 12, 15, 9, 11, 7, 13, 12,
		11, 13, 6, 7, 14, 9, 13, 15, 14, 8, 13, 6, 5, 12, 7, 5,
		11, 12, 14, 15, 14, 15, 9, 8, 9, 14, 5, 6, 8, 6, 5, 12,
		9, 15, 5, 11, 6, 8, 13, 12, 5, 12, 13, 14, 11, 8, 5, 6,
	}
	rmdSR = []uint{
		8, 9, 9, 11, 13, 15, 15, 5, 7, 7, 8, 11, 14, 14, 12, 6,
		9, 13, 15, 7, 12, 8, 9, 11, 7, 7, 12, 7, 6, 15, 13, 11,
		9, 7, 15, 11, 8, 6, 6, 14, 12, 13, 5, 14, 13, 13, 7, 5,
		15, 5, 8, 11, 14, 14, 6, 14, 6, 9, 12, 9, 12, 5, 15, 8,
		8, 5, 12, 9, 12, 5, 14, 6, 8, 13, 6, 5, 15, 13, 11, 11,
	}
)

// 左线五轮的加性常数（按轮次块 i/16）。
func leftK(round int) uint32 {
	switch round {
	case 1:
		return 0x5a827999
	case 2:
		return 0x6ed9eba1
	case 3:
		return 0x8f1bbcdc
	case 4:
		return 0xa953fd4e
	default:
		return 0
	}
}

// 右线五轮的加性常数（按轮次块 i/16，与左线不同且末轮为 0）。
func rightK(round int) uint32 {
	switch round {
	case 0:
		return 0x50a28be6
	case 1:
		return 0x5c4dd124
	case 2:
		return 0x6d703ef3
	case 3:
		return 0x7a6d76e9
	default:
		return 0
	}
}

// 左线布尔函数（按轮次块 i/16）。
func leftF(round int, b, c, d uint32) uint32 {
	switch round {
	case 0:
		return b ^ c ^ d
	case 1:
		return b&c | ^b&d
	case 2:
		return (b | ^c) ^ d
	case 3:
		return b&d | c&^d
	default:
		return b ^ (c | ^d)
	}
}

// 右线布尔函数（按轮次块 i/16；与左线顺序相反：块 k 取左线第 4-k 支）。
func rightF(round int, b, c, d uint32) uint32 {
	switch round {
	case 0:
		return b ^ (c | ^d)
	case 1:
		return b&d | c&^d
	case 2:
		return (b | ^c) ^ d
	case 3:
		return b&c | ^b&d
	default:
		return b ^ c ^ d
	}
}

// ripemd160Sum 返回 data 的 RIPEMD-160 摘要（20 字节）。
func ripemd160Sum(data []byte) []byte {
	h := [5]uint32{0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0}

	// 填充：0x80 + 0 对齐到 56 mod 64 + 64 位小端比特长度。
	ml := uint64(len(data)) * 8
	msg := append(append([]byte{}, data...), 0x80)
	for len(msg)%64 != 56 {
		msg = append(msg, 0x00)
	}
	for i := 0; i < 8; i++ {
		msg = append(msg, byte(ml>>(8*i)))
	}

	for off := 0; off < len(msg); off += 64 {
		var x [16]uint32
		for i := 0; i < 16; i++ {
			x[i] = uint32(msg[off+4*i]) | uint32(msg[off+4*i+1])<<8 |
				uint32(msg[off+4*i+2])<<16 | uint32(msg[off+4*i+3])<<24
		}
		a, b, c, d, e := h[0], h[1], h[2], h[3], h[4]
		aa, bb, cc, dd, ee := a, b, c, d, e
		for i := 0; i < 80; i++ {
			// 左线
			alpha := a + leftF(i/16, b, c, d) + x[rmdKL[i]] + leftK(i/16)
			alpha = bits.RotateLeft32(alpha, int(rmdSL[i])) + e
			beta := bits.RotateLeft32(c, 10)
			a, b, c, d, e = e, alpha, b, beta, d
			// 右线（布尔函数按 i/16 取逆序的那一支；消息序/旋转用 i）。
			alpha2 := aa + rightF(i/16, bb, cc, dd) + x[rmdKR[i]] + rightK(i/16)
			alpha2 = bits.RotateLeft32(alpha2, int(rmdSR[i])) + ee
			beta2 := bits.RotateLeft32(cc, 10)
			aa, bb, cc, dd, ee = ee, alpha2, bb, beta2, dd
		}
		// 合并（与规范/参考实现一致的链式更新）。
		dd += c + h[1]
		h[1] = h[2] + d + ee
		h[2] = h[3] + e + aa
		h[3] = h[4] + a + bb
		h[4] = h[0] + b + cc
		h[0] = dd
	}

	out := make([]byte, 20)
	for i, v := range h {
		out[4*i] = byte(v)
		out[4*i+1] = byte(v >> 8)
		out[4*i+2] = byte(v >> 16)
		out[4*i+3] = byte(v >> 24)
	}
	return out
}
