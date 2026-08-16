package keccak

import (
	"encoding/hex"
	"strings"
	"testing"
)

// 已知 Keccak-256 向量（注意：与 SHA3-256 不同，以太坊使用最初的 Keccak 排布）。
var keccakVectors = []struct {
	in   string
	want string
}{
	{"", "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"},
	{"abc", "4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45"},
	{"hello", "1c8aff950685c2ed4bc3174f3472287b56d9517b9c948127319a09a7a36deac8"},
}

func TestSum256Vectors(t *testing.T) {
	for _, v := range keccakVectors {
		got := Sum256([]byte(v.in))
		if hex.EncodeToString(got[:]) != v.want {
			t.Fatalf("Keccak256(%q) = %s, want %s", v.in, hex.EncodeToString(got[:]), v.want)
		}
	}
}

func TestDigestVectors(t *testing.T) {
	for _, v := range keccakVectors {
		h := New()
		// 分块写入以覆盖缓冲/整块吸收路径。
		mid := len(v.in) / 2
		if _, err := h.Write([]byte(v.in[:mid])); err != nil {
			t.Fatalf("write1: %v", err)
		}
		if _, err := h.Write([]byte(v.in[mid:])); err != nil {
			t.Fatalf("write2: %v", err)
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != v.want {
			t.Fatalf("incremental Keccak256(%q) = %s, want %s", v.in, got, v.want)
		}
		// Sum 不破坏状态，可重复调用。
		if got2 := hex.EncodeToString(h.Sum(nil)); got2 != v.want {
			t.Fatalf("repeat Sum mismatch: %s != %s", got2, v.want)
		}
	}
}

// TestKeccakNotSHA3 强调本实现与 SHA3 输出不同（防止误用 crypto/sha3 上链）。
func TestKeccakNotSHA3(t *testing.T) {
	// "abc" 的 SHA3-256 为 3a985da74f...
	abc := Sum256([]byte("abc"))
	if strings.HasPrefix(hex.EncodeToString(abc[:]), "3a985da7") {
		t.Fatalf("Keccak must differ from SHA3-256 for eth compatibility")
	}
}
