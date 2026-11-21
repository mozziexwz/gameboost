// Package probe implements the launcher's network checks, including exit
// IPv4 lookup, node TCP latency, and proxied HTTPS response latency.
package probe

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// DefaultSampleCount is the number of attempts per latency check.
const DefaultSampleCount = 3

// Sample is one latency attempt.
type Sample struct {
	OK       bool
	Duration time.Duration
	Err      string
}

// Result aggregates attempts. Failed attempts are never counted as zero.
type Result struct {
	Samples      []Sample
	SuccessCount int
	TotalCount   int
	Average      time.Duration
	Errors       []string // unique failure reasons in first-seen order
	fastest      bool
}

// NewResult aggregates samples; failed attempts never count as zero.
func NewResult(samples []Sample) Result {
	r := Result{Samples: samples, TotalCount: len(samples)}
	var sum time.Duration
	seen := make(map[string]bool)
	for _, s := range samples {
		if s.OK {
			r.SuccessCount++
			sum += s.Duration
			continue
		}
		if !seen[s.Err] {
			seen[s.Err] = true
			r.Errors = append(r.Errors, s.Err)
		}
	}
	if r.SuccessCount > 0 {
		r.Average = sum / time.Duration(r.SuccessCount)
	}
	return r
}

// Unavailable is the result of a check that could not run at all.
func Unavailable(reason string) Result {
	return Result{TotalCount: DefaultSampleCount, Errors: []string{reason}}
}

func (r Result) displayLatency() time.Duration {
	elapsed := r.Average
	if r.fastest {
		for _, s := range r.Samples {
			if s.OK && s.Duration < elapsed {
				elapsed = s.Duration
			}
		}
	}
	return elapsed
}

// Compact formats like the legacy launcher: "191 ms", "<1 ms" or "未测得：原因".
func (r Result) Compact() string {
	if r.SuccessCount > 0 {
		ms := float64(r.displayLatency()) / float64(time.Millisecond)
		if ms < 0.5 {
			return "<1 ms"
		}
		return fmt.Sprintf("%d ms", int64(math.Round(ms)))
	}
	return "未测得：" + strings.Join(r.Errors, "、")
}

// Detail adds the success count, like the legacy detailed format.
func (r Result) Detail() string {
	counts := fmt.Sprintf("成功 %d/%d", r.SuccessCount, r.TotalCount)
	if r.SuccessCount == 0 {
		return fmt.Sprintf("未测得（%s；%s）", counts, strings.Join(r.Errors, "、"))
	}
	if r.fastest {
		counts = "取最快响应，" + counts
	}
	ms := float64(r.displayLatency()) / float64(time.Millisecond)
	text := fmt.Sprintf("%.1f ms（%s）", ms, counts)
	if ms < 0.05 {
		text = fmt.Sprintf("<0.1 ms（%s）", counts)
	}
	if r.SuccessCount < r.TotalCount {
		text += "；部分失败：" + strings.Join(r.Errors, "、")
	}
	return text
}
