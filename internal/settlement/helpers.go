package settlement

import (
	"context"
	"sync"
	"time"
)

// subscriberSet 管理一组事件订阅 channel，提供注册、按 context 取消自动注销，以及带背压
// 超时的广播。充值（DepositEvent）与提现（WithdrawEvent）两套网关的订阅/广播逻辑完全相同，
// 抽此泛型避免 MockChainGateway / MockWithdrawGateway 各复制一份 Watch/emit 实现（#8）。
type subscriberSet[T any] struct {
	mu   sync.Mutex
	subs []chan T
}

// register 追加订阅 channel，并在 ctx 结束时自动将其从集合中移除。注销由内部 goroutine 在
// ctx.Done 时执行，调用方无需手动清理。
func (s *subscriberSet[T]) register(ch chan T, ctx context.Context) {
	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		for i, sub := range s.subs {
			if sub == ch {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
}

// broadcast 向所有订阅者投递事件；订阅者背压超过 emitSendTimeout 则放弃本次投递并调用
// onDrop 告警（非静默）。事件状态仍持久化于网关 pending，可经 Pending()/WithdrawHistory() 对账。
func (s *subscriberSet[T]) broadcast(ev T, onDrop func(ev T)) {
	s.mu.Lock()
	subs := make([]chan T, len(s.subs))
	copy(subs, s.subs)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		case <-time.After(emitSendTimeout):
			if onDrop != nil {
				onDrop(ev)
			}
		}
	}
}

// nextConfirmations 返回事件的新确认数：有 confirmSource 且查询成功→用真实链上确认数；
// 否则（无 source 或查询失败）→回退模拟「当前 +1」（fail-degraded）。充值/提现网关共用，
// 消除两处逐字复制的 realConfirmations（#8）。
func nextConfirmations(cs ConfirmSource, ctx context.Context, chain Chain, txHash string, current int) int {
	if cs != nil {
		if c, err := cs.Confirmations(ctx, chain, txHash); err == nil {
			return c
		}
	}
	return current + 1
}
