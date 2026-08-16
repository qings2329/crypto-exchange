package settlement

import (
	"sync"
	"testing"
)

// TestDepositAddrGenConcurrent 验证全局生成器读写经 atomic.Pointer 同步，运行时并发重配
// （SetDepositAddressGenerator）与 GenerateAddress 读取无 data race（#7）。
func TestDepositAddrGenConcurrent(t *testing.T) {
	xpub := deriveTestXPUB(t)
	gen, err := NewDepositAddressGenerator(DepositConfig{XPUB: xpub, BTCAddressType: "p2wpkh"})
	if err != nil {
		t.Fatalf("NewDepositAddressGenerator: %v", err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { SetDepositAddressGenerator(nil) }) // 并发重配后还原，避免泄漏到后续用例
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				SetDepositAddressGenerator(gen)
				_ = GenerateAddress(int64(j), ChainETH)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				SetDepositAddressGenerator(nil)
				_ = GenerateAddress(1, ChainETH)
			}
		}()
	}
	wg.Wait()
}
