// Package keccak 提供 Keccak-256（以太坊沿用的最初 Keccak 排布，非 SHA3-256）的纯 Go
// 实现，用于以太坊交易摘要与地址派生。与标准库 crypto/sha3 不同：Keccak 的填充域字节是
// 0x01（SHA3 为 0x06），二者输出不同，故以太坊场景必须用本实现而非 SHA3。
//
// 算法：Keccak-f[1600] 海绵结构，rate=1088bit(136B)、capacity=512bit、输出 256bit。
package keccak

import (
	"encoding/binary"
	"math/bits"
)

// rate 是 Keccak-256 的吸收速率（字节）：1088 bit。
const rate = 136

// rc 是 Keccak-f[1600] 的 24 轮轮常量。
var rc = [...]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// keccakF 原地执行 Keccak-f[1600] 置换（24 轮）。状态 a 以 lane 索引 0..24 布局，
// 与 Keccak 规范及 golang.org/x/crypto/sha3 的内核一致（仅填充字节不同：Keccak 用 0x01）。
// 实现翻译自 Keccak 参考代码（Keccak-inplace.c），经已知测试向量验证。
func keccakF(a *[25]uint64) {
	var t, bc0, bc1, bc2, bc3, bc4, d0, d1, d2, d3, d4 uint64
	for i := 0; i < 24; i += 4 {
		// Round 1
		bc0 = a[0] ^ a[5] ^ a[10] ^ a[15] ^ a[20]
		bc1 = a[1] ^ a[6] ^ a[11] ^ a[16] ^ a[21]
		bc2 = a[2] ^ a[7] ^ a[12] ^ a[17] ^ a[22]
		bc3 = a[3] ^ a[8] ^ a[13] ^ a[18] ^ a[23]
		bc4 = a[4] ^ a[9] ^ a[14] ^ a[19] ^ a[24]
		d0 = bc4 ^ (bc1<<1 | bc1>>63)
		d1 = bc0 ^ (bc2<<1 | bc2>>63)
		d2 = bc1 ^ (bc3<<1 | bc3>>63)
		d3 = bc2 ^ (bc4<<1 | bc4>>63)
		d4 = bc3 ^ (bc0<<1 | bc0>>63)

		bc0 = a[0] ^ d0
		t = a[6] ^ d1
		bc1 = bits.RotateLeft64(t, 44)
		t = a[12] ^ d2
		bc2 = bits.RotateLeft64(t, 43)
		t = a[18] ^ d3
		bc3 = bits.RotateLeft64(t, 21)
		t = a[24] ^ d4
		bc4 = bits.RotateLeft64(t, 14)
		a[0] = bc0 ^ (bc2 &^ bc1) ^ rc[i]
		a[6] = bc1 ^ (bc3 &^ bc2)
		a[12] = bc2 ^ (bc4 &^ bc3)
		a[18] = bc3 ^ (bc0 &^ bc4)
		a[24] = bc4 ^ (bc1 &^ bc0)

		t = a[10] ^ d0
		bc2 = bits.RotateLeft64(t, 3)
		t = a[16] ^ d1
		bc3 = bits.RotateLeft64(t, 45)
		t = a[22] ^ d2
		bc4 = bits.RotateLeft64(t, 61)
		t = a[3] ^ d3
		bc0 = bits.RotateLeft64(t, 28)
		t = a[9] ^ d4
		bc1 = bits.RotateLeft64(t, 20)
		a[10] = bc0 ^ (bc2 &^ bc1)
		a[16] = bc1 ^ (bc3 &^ bc2)
		a[22] = bc2 ^ (bc4 &^ bc3)
		a[3] = bc3 ^ (bc0 &^ bc4)
		a[9] = bc4 ^ (bc1 &^ bc0)

		t = a[20] ^ d0
		bc4 = bits.RotateLeft64(t, 18)
		t = a[1] ^ d1
		bc0 = bits.RotateLeft64(t, 1)
		t = a[7] ^ d2
		bc1 = bits.RotateLeft64(t, 6)
		t = a[13] ^ d3
		bc2 = bits.RotateLeft64(t, 25)
		t = a[19] ^ d4
		bc3 = bits.RotateLeft64(t, 8)
		a[20] = bc0 ^ (bc2 &^ bc1)
		a[1] = bc1 ^ (bc3 &^ bc2)
		a[7] = bc2 ^ (bc4 &^ bc3)
		a[13] = bc3 ^ (bc0 &^ bc4)
		a[19] = bc4 ^ (bc1 &^ bc0)

		t = a[5] ^ d0
		bc1 = bits.RotateLeft64(t, 36)
		t = a[11] ^ d1
		bc2 = bits.RotateLeft64(t, 10)
		t = a[17] ^ d2
		bc3 = bits.RotateLeft64(t, 15)
		t = a[23] ^ d3
		bc4 = bits.RotateLeft64(t, 56)
		t = a[4] ^ d4
		bc0 = bits.RotateLeft64(t, 27)
		a[5] = bc0 ^ (bc2 &^ bc1)
		a[11] = bc1 ^ (bc3 &^ bc2)
		a[17] = bc2 ^ (bc4 &^ bc3)
		a[23] = bc3 ^ (bc0 &^ bc4)
		a[4] = bc4 ^ (bc1 &^ bc0)

		t = a[15] ^ d0
		bc3 = bits.RotateLeft64(t, 41)
		t = a[21] ^ d1
		bc4 = bits.RotateLeft64(t, 2)
		t = a[2] ^ d2
		bc0 = bits.RotateLeft64(t, 62)
		t = a[8] ^ d3
		bc1 = bits.RotateLeft64(t, 55)
		t = a[14] ^ d4
		bc2 = bits.RotateLeft64(t, 39)
		a[15] = bc0 ^ (bc2 &^ bc1)
		a[21] = bc1 ^ (bc3 &^ bc2)
		a[2] = bc2 ^ (bc4 &^ bc3)
		a[8] = bc3 ^ (bc0 &^ bc4)
		a[14] = bc4 ^ (bc1 &^ bc0)

		// Round 2
		bc0 = a[0] ^ a[5] ^ a[10] ^ a[15] ^ a[20]
		bc1 = a[1] ^ a[6] ^ a[11] ^ a[16] ^ a[21]
		bc2 = a[2] ^ a[7] ^ a[12] ^ a[17] ^ a[22]
		bc3 = a[3] ^ a[8] ^ a[13] ^ a[18] ^ a[23]
		bc4 = a[4] ^ a[9] ^ a[14] ^ a[19] ^ a[24]
		d0 = bc4 ^ (bc1<<1 | bc1>>63)
		d1 = bc0 ^ (bc2<<1 | bc2>>63)
		d2 = bc1 ^ (bc3<<1 | bc3>>63)
		d3 = bc2 ^ (bc4<<1 | bc4>>63)
		d4 = bc3 ^ (bc0<<1 | bc0>>63)

		bc0 = a[0] ^ d0
		t = a[16] ^ d1
		bc1 = bits.RotateLeft64(t, 44)
		t = a[7] ^ d2
		bc2 = bits.RotateLeft64(t, 43)
		t = a[23] ^ d3
		bc3 = bits.RotateLeft64(t, 21)
		t = a[14] ^ d4
		bc4 = bits.RotateLeft64(t, 14)
		a[0] = bc0 ^ (bc2 &^ bc1) ^ rc[i+1]
		a[16] = bc1 ^ (bc3 &^ bc2)
		a[7] = bc2 ^ (bc4 &^ bc3)
		a[23] = bc3 ^ (bc0 &^ bc4)
		a[14] = bc4 ^ (bc1 &^ bc0)

		t = a[20] ^ d0
		bc2 = bits.RotateLeft64(t, 3)
		t = a[11] ^ d1
		bc3 = bits.RotateLeft64(t, 45)
		t = a[2] ^ d2
		bc4 = bits.RotateLeft64(t, 61)
		t = a[18] ^ d3
		bc0 = bits.RotateLeft64(t, 28)
		t = a[9] ^ d4
		bc1 = bits.RotateLeft64(t, 20)
		a[20] = bc0 ^ (bc2 &^ bc1)
		a[11] = bc1 ^ (bc3 &^ bc2)
		a[2] = bc2 ^ (bc4 &^ bc3)
		a[18] = bc3 ^ (bc0 &^ bc4)
		a[9] = bc4 ^ (bc1 &^ bc0)

		t = a[15] ^ d0
		bc4 = bits.RotateLeft64(t, 18)
		t = a[6] ^ d1
		bc0 = bits.RotateLeft64(t, 1)
		t = a[22] ^ d2
		bc1 = bits.RotateLeft64(t, 6)
		t = a[13] ^ d3
		bc2 = bits.RotateLeft64(t, 25)
		t = a[4] ^ d4
		bc3 = bits.RotateLeft64(t, 8)
		a[15] = bc0 ^ (bc2 &^ bc1)
		a[6] = bc1 ^ (bc3 &^ bc2)
		a[22] = bc2 ^ (bc4 &^ bc3)
		a[13] = bc3 ^ (bc0 &^ bc4)
		a[4] = bc4 ^ (bc1 &^ bc0)

		t = a[10] ^ d0
		bc1 = bits.RotateLeft64(t, 36)
		t = a[1] ^ d1
		bc2 = bits.RotateLeft64(t, 10)
		t = a[17] ^ d2
		bc3 = bits.RotateLeft64(t, 15)
		t = a[8] ^ d3
		bc4 = bits.RotateLeft64(t, 56)
		t = a[24] ^ d4
		bc0 = bits.RotateLeft64(t, 27)
		a[10] = bc0 ^ (bc2 &^ bc1)
		a[1] = bc1 ^ (bc3 &^ bc2)
		a[17] = bc2 ^ (bc4 &^ bc3)
		a[8] = bc3 ^ (bc0 &^ bc4)
		a[24] = bc4 ^ (bc1 &^ bc0)

		t = a[5] ^ d0
		bc3 = bits.RotateLeft64(t, 41)
		t = a[21] ^ d1
		bc4 = bits.RotateLeft64(t, 2)
		t = a[12] ^ d2
		bc0 = bits.RotateLeft64(t, 62)
		t = a[3] ^ d3
		bc1 = bits.RotateLeft64(t, 55)
		t = a[19] ^ d4
		bc2 = bits.RotateLeft64(t, 39)
		a[5] = bc0 ^ (bc2 &^ bc1)
		a[21] = bc1 ^ (bc3 &^ bc2)
		a[12] = bc2 ^ (bc4 &^ bc3)
		a[3] = bc3 ^ (bc0 &^ bc4)
		a[19] = bc4 ^ (bc1 &^ bc0)

		// Round 3
		bc0 = a[0] ^ a[5] ^ a[10] ^ a[15] ^ a[20]
		bc1 = a[1] ^ a[6] ^ a[11] ^ a[16] ^ a[21]
		bc2 = a[2] ^ a[7] ^ a[12] ^ a[17] ^ a[22]
		bc3 = a[3] ^ a[8] ^ a[13] ^ a[18] ^ a[23]
		bc4 = a[4] ^ a[9] ^ a[14] ^ a[19] ^ a[24]
		d0 = bc4 ^ (bc1<<1 | bc1>>63)
		d1 = bc0 ^ (bc2<<1 | bc2>>63)
		d2 = bc1 ^ (bc3<<1 | bc3>>63)
		d3 = bc2 ^ (bc4<<1 | bc4>>63)
		d4 = bc3 ^ (bc0<<1 | bc0>>63)

		bc0 = a[0] ^ d0
		t = a[11] ^ d1
		bc1 = bits.RotateLeft64(t, 44)
		t = a[22] ^ d2
		bc2 = bits.RotateLeft64(t, 43)
		t = a[8] ^ d3
		bc3 = bits.RotateLeft64(t, 21)
		t = a[19] ^ d4
		bc4 = bits.RotateLeft64(t, 14)
		a[0] = bc0 ^ (bc2 &^ bc1) ^ rc[i+2]
		a[11] = bc1 ^ (bc3 &^ bc2)
		a[22] = bc2 ^ (bc4 &^ bc3)
		a[8] = bc3 ^ (bc0 &^ bc4)
		a[19] = bc4 ^ (bc1 &^ bc0)

		t = a[15] ^ d0
		bc2 = bits.RotateLeft64(t, 3)
		t = a[1] ^ d1
		bc3 = bits.RotateLeft64(t, 45)
		t = a[12] ^ d2
		bc4 = bits.RotateLeft64(t, 61)
		t = a[23] ^ d3
		bc0 = bits.RotateLeft64(t, 28)
		t = a[9] ^ d4
		bc1 = bits.RotateLeft64(t, 20)
		a[15] = bc0 ^ (bc2 &^ bc1)
		a[1] = bc1 ^ (bc3 &^ bc2)
		a[12] = bc2 ^ (bc4 &^ bc3)
		a[23] = bc3 ^ (bc0 &^ bc4)
		a[9] = bc4 ^ (bc1 &^ bc0)

		t = a[5] ^ d0
		bc4 = bits.RotateLeft64(t, 18)
		t = a[16] ^ d1
		bc0 = bits.RotateLeft64(t, 1)
		t = a[2] ^ d2
		bc1 = bits.RotateLeft64(t, 6)
		t = a[13] ^ d3
		bc2 = bits.RotateLeft64(t, 25)
		t = a[24] ^ d4
		bc3 = bits.RotateLeft64(t, 8)
		a[5] = bc0 ^ (bc2 &^ bc1)
		a[16] = bc1 ^ (bc3 &^ bc2)
		a[2] = bc2 ^ (bc4 &^ bc3)
		a[13] = bc3 ^ (bc0 &^ bc4)
		a[24] = bc4 ^ (bc1 &^ bc0)

		t = a[20] ^ d0
		bc1 = bits.RotateLeft64(t, 36)
		t = a[6] ^ d1
		bc2 = bits.RotateLeft64(t, 10)
		t = a[17] ^ d2
		bc3 = bits.RotateLeft64(t, 15)
		t = a[3] ^ d3
		bc4 = bits.RotateLeft64(t, 56)
		t = a[14] ^ d4
		bc0 = bits.RotateLeft64(t, 27)
		a[20] = bc0 ^ (bc2 &^ bc1)
		a[6] = bc1 ^ (bc3 &^ bc2)
		a[17] = bc2 ^ (bc4 &^ bc3)
		a[3] = bc3 ^ (bc0 &^ bc4)
		a[14] = bc4 ^ (bc1 &^ bc0)

		t = a[10] ^ d0
		bc3 = bits.RotateLeft64(t, 41)
		t = a[21] ^ d1
		bc4 = bits.RotateLeft64(t, 2)
		t = a[7] ^ d2
		bc0 = bits.RotateLeft64(t, 62)
		t = a[18] ^ d3
		bc1 = bits.RotateLeft64(t, 55)
		t = a[4] ^ d4
		bc2 = bits.RotateLeft64(t, 39)
		a[10] = bc0 ^ (bc2 &^ bc1)
		a[21] = bc1 ^ (bc3 &^ bc2)
		a[7] = bc2 ^ (bc4 &^ bc3)
		a[18] = bc3 ^ (bc0 &^ bc4)
		a[4] = bc4 ^ (bc1 &^ bc0)

		// Round 4
		bc0 = a[0] ^ a[5] ^ a[10] ^ a[15] ^ a[20]
		bc1 = a[1] ^ a[6] ^ a[11] ^ a[16] ^ a[21]
		bc2 = a[2] ^ a[7] ^ a[12] ^ a[17] ^ a[22]
		bc3 = a[3] ^ a[8] ^ a[13] ^ a[18] ^ a[23]
		bc4 = a[4] ^ a[9] ^ a[14] ^ a[19] ^ a[24]
		d0 = bc4 ^ (bc1<<1 | bc1>>63)
		d1 = bc0 ^ (bc2<<1 | bc2>>63)
		d2 = bc1 ^ (bc3<<1 | bc3>>63)
		d3 = bc2 ^ (bc4<<1 | bc4>>63)
		d4 = bc3 ^ (bc0<<1 | bc0>>63)

		bc0 = a[0] ^ d0
		t = a[1] ^ d1
		bc1 = bits.RotateLeft64(t, 44)
		t = a[2] ^ d2
		bc2 = bits.RotateLeft64(t, 43)
		t = a[3] ^ d3
		bc3 = bits.RotateLeft64(t, 21)
		t = a[4] ^ d4
		bc4 = bits.RotateLeft64(t, 14)
		a[0] = bc0 ^ (bc2 &^ bc1) ^ rc[i+3]
		a[1] = bc1 ^ (bc3 &^ bc2)
		a[2] = bc2 ^ (bc4 &^ bc3)
		a[3] = bc3 ^ (bc0 &^ bc4)
		a[4] = bc4 ^ (bc1 &^ bc0)

		t = a[5] ^ d0
		bc2 = bits.RotateLeft64(t, 3)
		t = a[6] ^ d1
		bc3 = bits.RotateLeft64(t, 45)
		t = a[7] ^ d2
		bc4 = bits.RotateLeft64(t, 61)
		t = a[8] ^ d3
		bc0 = bits.RotateLeft64(t, 28)
		t = a[9] ^ d4
		bc1 = bits.RotateLeft64(t, 20)
		a[5] = bc0 ^ (bc2 &^ bc1)
		a[6] = bc1 ^ (bc3 &^ bc2)
		a[7] = bc2 ^ (bc4 &^ bc3)
		a[8] = bc3 ^ (bc0 &^ bc4)
		a[9] = bc4 ^ (bc1 &^ bc0)

		t = a[10] ^ d0
		bc4 = bits.RotateLeft64(t, 18)
		t = a[11] ^ d1
		bc0 = bits.RotateLeft64(t, 1)
		t = a[12] ^ d2
		bc1 = bits.RotateLeft64(t, 6)
		t = a[13] ^ d3
		bc2 = bits.RotateLeft64(t, 25)
		t = a[14] ^ d4
		bc3 = bits.RotateLeft64(t, 8)
		a[10] = bc0 ^ (bc2 &^ bc1)
		a[11] = bc1 ^ (bc3 &^ bc2)
		a[12] = bc2 ^ (bc4 &^ bc3)
		a[13] = bc3 ^ (bc0 &^ bc4)
		a[14] = bc4 ^ (bc1 &^ bc0)

		t = a[15] ^ d0
		bc1 = bits.RotateLeft64(t, 36)
		t = a[16] ^ d1
		bc2 = bits.RotateLeft64(t, 10)
		t = a[17] ^ d2
		bc3 = bits.RotateLeft64(t, 15)
		t = a[18] ^ d3
		bc4 = bits.RotateLeft64(t, 56)
		t = a[19] ^ d4
		bc0 = bits.RotateLeft64(t, 27)
		a[15] = bc0 ^ (bc2 &^ bc1)
		a[16] = bc1 ^ (bc3 &^ bc2)
		a[17] = bc2 ^ (bc4 &^ bc3)
		a[18] = bc3 ^ (bc0 &^ bc4)
		a[19] = bc4 ^ (bc1 &^ bc0)

		t = a[20] ^ d0
		bc3 = bits.RotateLeft64(t, 41)
		t = a[21] ^ d1
		bc4 = bits.RotateLeft64(t, 2)
		t = a[22] ^ d2
		bc0 = bits.RotateLeft64(t, 62)
		t = a[23] ^ d3
		bc1 = bits.RotateLeft64(t, 55)
		t = a[24] ^ d4
		bc2 = bits.RotateLeft64(t, 39)
		a[20] = bc0 ^ (bc2 &^ bc1)
		a[21] = bc1 ^ (bc3 &^ bc2)
		a[22] = bc2 ^ (bc4 &^ bc3)
		a[23] = bc3 ^ (bc0 &^ bc4)
		a[24] = bc4 ^ (bc1 &^ bc0)
	}
}

// Sum256 计算 Keccak-256 摘要（32 字节）。
func Sum256(data []byte) [32]byte {
	var st [25]uint64
	// 吸收完整块。
	for len(data) >= rate {
		for i := 0; i < rate/8; i++ {
			st[i] ^= binary.LittleEndian.Uint64(data[i*8:])
		}
		keccakF(&st)
		data = data[rate:]
	}
	// 末块：复制剩余数据，加 Keccak 填充（0x01 … 0x80）。
	var block [rate]byte
	copy(block[:], data)
	block[len(data)] = 0x01
	block[rate-1] |= 0x80
	for i := 0; i < rate/8; i++ {
		st[i] ^= binary.LittleEndian.Uint64(block[i*8:])
	}
	keccakF(&st)
	// 挤出：rate(136) >= 32，单次即可。
	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], st[i])
	}
	return out
}

// New 返回可增量写入的 Keccak-256 哈希器（与 crypto/sha256 同款接口）。
func New() *Digest {
	return &Digest{}
}

// Digest 是增量式 Keccak-256 计算（内部缓冲未吸收的尾部数据）。
type Digest struct {
	st   [25]uint64
	buf  [rate]byte
	nbuf int
}

// Write 追加数据，返回成功写入的字节数（恒等于 len(p)）。
func (d *Digest) Write(p []byte) (int, error) {
	total := len(p)
	if d.nbuf > 0 {
		n := copy(d.buf[d.nbuf:], p)
		d.nbuf += n
		p = p[n:]
		if d.nbuf < rate {
			return total, nil
		}
		d.absorbBlock(d.buf[:])
		d.nbuf = 0
	}
	// 整块吸收。
	for len(p) >= rate {
		d.absorbBlock(p[:rate])
		p = p[rate:]
	}
	// 剩余尾部留在缓冲。
	if len(p) > 0 {
		d.nbuf = copy(d.buf[:], p)
	}
	return total, nil
}

func (d *Digest) absorbBlock(block []byte) {
	for i := 0; i < rate/8; i++ {
		d.st[i] ^= binary.LittleEndian.Uint64(block[i*8:])
	}
	keccakF(&d.st)
}

// Size 返回摘要长度（32 字节），满足 hash.Hash 接口。
func (d *Digest) Size() int { return 32 }

// BlockSize 返回吸收速率（136 字节），满足 hash.Hash 接口。
func (d *Digest) BlockSize() int { return rate }

// Sum 收尾并返回 32 字节摘要（不修改 d，可重复调用）。
func (d *Digest) Sum(in []byte) []byte {
	var st [25]uint64
	copy(st[:], d.st[:])
	var block [rate]byte
	copy(block[:], d.buf[:d.nbuf])
	block[d.nbuf] = 0x01
	block[rate-1] |= 0x80
	for i := 0; i < rate/8; i++ {
		st[i] ^= binary.LittleEndian.Uint64(block[i*8:])
	}
	keccakF(&st)
	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], st[i])
	}
	return append(in, out[:]...)
}
