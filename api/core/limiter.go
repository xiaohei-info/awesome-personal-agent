package core

import (
	"sync"
	"time"
)

// 登录限速（§14.13）。计数只在内存里：重启清零，不进账本。
const (
	lockAfterFails = 5                // 同一来源地址连续失败这么多次后锁定
	lockFor        = 15 * time.Minute // 锁定时长
	floodWindow    = 15 * time.Minute // 全部地址失败次数的统计窗口
	floodAbove     = 20               // 窗口内失败超过这个数，所有登录尝试延迟应答并告警
)

type addrState struct {
	fails  int       // 连续失败次数；成功或锁定即清零
	flight int       // 已放行、结果未定的尝试数
	last   time.Time // 最近一次失败
	locked time.Time // 锁定到何时；零值未锁定
}

type limiter struct {
	mu      sync.Mutex
	addrs   map[string]*addrState
	fails   []time.Time // 窗口内全部地址的失败时刻
	alerted bool        // 本轮超限已告警；失败数回落到阈值内后复位，下次超限再告警
}

func newLimiter() *limiter { return &limiter{addrs: map[string]*addrState{}} }

// begin 在校验口令之前预留一次尝试，检查与计数在同一把锁里，并发请求无法绕过：
// 地址仍在锁定期，或「已失败 + 在途」已达上限（在途的若都失败即会触发锁定），返回需要等待的时长，不放行；
// 放行时返回 0，调用方必须在结果记完后调用 end。
func (l *limiter) begin(addr string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.addrs[addr]
	if a == nil {
		a = &addrState{}
		l.addrs[addr] = a
	}
	if now.Before(a.locked) {
		return a.locked.Sub(now)
	}
	if a.fails+a.flight >= lockAfterFails {
		return time.Second
	}
	a.flight++
	return 0
}

// end 释放 begin 预留的名额。
func (l *limiter) end(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.addrs[addr]; a != nil && a.flight > 0 {
		a.flight--
	}
}

// flooded 报告窗口内全部地址的失败是否已超过阈值。
func (l *limiter) flooded(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trim(now)
	return len(l.fails) > floodAbove
}

// failure 记一次失败。locked 为真表示这次失败触发了锁定；flood > 0 表示这次失败让窗口内总数首次超限（要告警一次）。
func (l *limiter) failure(addr string, now time.Time) (locked bool, until time.Time, flood int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trim(now)
	a := l.addrs[addr]
	if a == nil {
		a = &addrState{}
		l.addrs[addr] = a
	}
	a.fails++
	a.last = now
	if a.fails >= lockAfterFails {
		a.fails, a.locked = 0, now.Add(lockFor)
		locked, until = true, a.locked
	}
	l.fails = append(l.fails, now)
	if len(l.fails) > floodAbove && !l.alerted {
		l.alerted, flood = true, len(l.fails)
	}
	l.prune(now)
	return
}

// success 清掉 addr 的连续失败计数。
func (l *limiter) success(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.addrs[addr]; a != nil {
		a.fails = 0
	}
}

func (l *limiter) trim(now time.Time) {
	i := 0
	for i < len(l.fails) && !l.fails[i].After(now.Add(-floodWindow)) {
		i++
	}
	l.fails = l.fails[i:]
	if len(l.fails) <= floodAbove {
		l.alerted = false
	}
}

// prune 丢掉不再有影响的地址记录，防止攻击者换地址撑大内存。
func (l *limiter) prune(now time.Time) {
	if len(l.addrs) < 1024 {
		return
	}
	for k, a := range l.addrs {
		if a.flight == 0 && !now.Before(a.locked) && now.Sub(a.last) >= floodWindow {
			delete(l.addrs, k)
		}
	}
}
