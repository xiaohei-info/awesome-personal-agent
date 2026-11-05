package clock

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule 是周期定时的节拍：固定时长（如 "24h"），或标准 5 段 cron（分 时 日 月 周，支持 * , - / 与 */n、a-b/n）。
// cron 可带 "CRON_TZ=<IANA 时区> " 前缀指定时区，缺省取本机时区。
// 日与周都有限定时按标准 cron 取并集（任一满足即触发）；周 0 与 7 都是周日。
type Schedule struct {
	every time.Duration // 非零：固定时长
	loc   *time.Location
	// 分 时 日 月 周 的允许值位集
	min, hour, dom, mon, dow uint64
	domStar, dowStar         bool
}

// ParseSchedule 解析时长或 cron 表达式。
func ParseSchedule(s string) (Schedule, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return Schedule{}, invalid("recurrence %q is not a positive duration", s)
		}
		return Schedule{every: d}, nil
	}
	sc := Schedule{loc: time.Local}
	if rest, ok := strings.CutPrefix(s, "CRON_TZ="); ok {
		zone, expr, _ := strings.Cut(rest, " ")
		loc, err := time.LoadLocation(zone)
		if err != nil || zone == "" {
			return Schedule{}, invalid("recurrence %q has an unknown time zone", s)
		}
		sc.loc, s = loc, expr
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return Schedule{}, invalid("recurrence %q is neither a positive duration like 24h nor a 5-field cron expression", s)
	}
	var err error
	for _, p := range []struct {
		dst      *uint64
		lo, hi   int
		fieldStr string
	}{{&sc.min, 0, 59, f[0]}, {&sc.hour, 0, 23, f[1]}, {&sc.dom, 1, 31, f[2]}, {&sc.mon, 1, 12, f[3]}, {&sc.dow, 0, 7, f[4]}} {
		if *p.dst, err = parseField(p.fieldStr, p.lo, p.hi); err != nil {
			return Schedule{}, invalid("recurrence %q: %v", s, err)
		}
	}
	sc.domStar, sc.dowStar = strings.HasPrefix(f[2], "*"), strings.HasPrefix(f[4], "*")
	if sc.dow&(1<<7) != 0 { // 7 即周日
		sc.dow |= 1
	}
	return sc, nil
}

// parseField 解析一段：逗号分隔的 "*"、"n"、"a-b"，各自可带 "/step"。
func parseField(s string, lo, hi int) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			var err error
			if step, err = strconv.Atoi(stepS); err != nil || step <= 0 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
		}
		a, b := lo, hi
		if rng != "*" {
			from, to, isRange := strings.Cut(rng, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(from)
			b = a
			if isRange {
				b, err2 = strconv.Atoi(to)
			} else if hasStep {
				b = hi // "n/step" 等价 "n-hi/step"
			}
			if err1 != nil || err2 != nil || a < lo || b > hi || a > b {
				return 0, fmt.Errorf("bad value %q (allowed %d-%d)", part, lo, hi)
			}
		}
		for v := a; v <= b; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

// Next 返回严格晚于 after 的下一个节拍。时长是 after + 时长；cron 是下一个匹配的整分钟。
// 夏令时跳过的时刻不触发，重复的时刻按墙上时间只触发一次（逐分钟推进，取到的第一个匹配）。
func (s Schedule) Next(after time.Time) time.Time {
	if s.every > 0 {
		return after.Add(s.every)
	}
	t := after.In(s.loc).Truncate(time.Minute).Add(time.Minute)
	// 最长 8 年（覆盖 2 月 29 日）：不匹配的表达式（如 2 月 31 日）找不到时返回零值。
	for end := t.AddDate(8, 0, 0); t.Before(end); {
		switch {
		case s.mon&(1<<int(t.Month())) == 0: // 跳到下个月 1 日
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, s.loc)
		case !s.dayOK(t): // 跳到次日 0 点
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, s.loc)
		case s.hour&(1<<t.Hour()) == 0: // 跳到下一小时
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, s.loc)
		case s.min&(1<<t.Minute()) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

func (s Schedule) dayOK(t time.Time) bool {
	d, w := s.dom&(1<<t.Day()) != 0, s.dow&(1<<int(t.Weekday())) != 0
	if s.domStar || s.dowStar {
		return d && w
	}
	return d || w
}

// Start 是周期定时从 now 起算的第一个节拍。
func Start(spec string, now time.Time) (time.Time, error) {
	s, err := ParseSchedule(spec)
	if err != nil {
		return time.Time{}, err
	}
	at := s.Next(now)
	if at.IsZero() {
		return at, invalid("recurrence %q never fires", spec)
	}
	return at.UTC(), nil
}

// latest 返回 at 之后（含 at）最近一个不晚于 now 的节拍 due，以及其后的下一拍。at 是已到期的节拍。
func (s Schedule) latest(at, now time.Time) (due, next time.Time) {
	if s.every > 0 {
		due = at.Add(now.Sub(at) / s.every * s.every) // 已错过的整节拍数；只补最近一次，下一次落在原节拍上
		return due, due.Add(s.every)
	}
	// shortcut: 逐拍推进，停机很久的分钟级 cron 要循环多次（一年约 50 万次）；需要时改成按字段反推。
	due = at
	for {
		n := s.Next(due)
		if n.IsZero() || n.After(now) {
			return due, n
		}
		due = n
	}
}
