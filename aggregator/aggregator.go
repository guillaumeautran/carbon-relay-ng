package aggregator

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/graphite-ng/carbon-relay-ng/clock"
)

type Func func(in []float64) float64

func Avg(in []float64) float64 {
	if len(in) == 0 {
		panic("avg() called in aggregator with 0 terms")
	}
	return Sum(in) / float64(len(in))
}

func Delta(in []float64) float64 {
	if len(in) == 0 {
		panic("delta() called in aggregator with 0 terms")
	}
	min := in[0]
	max := in[0]
	for _, val := range in {
		if val > max {
			max = val
		} else if val < min {
			min = val
		}
	}
	return max - min
}

func Last(in []float64) float64 {
	if len(in) == 0 {
		panic("last() called in aggregator with 0 terms")
	}
	last := in[len(in)-1]
	return last
}

func Max(in []float64) float64 {
	if len(in) == 0 {
		panic("max() called in aggregator with 0 terms")
	}
	max := in[0]
	for _, val := range in {
		if val > max {
			max = val
		}
	}
	return max
}

func Min(in []float64) float64 {
	if len(in) == 0 {
		panic("min() called in aggregator with 0 terms")
	}
	min := in[0]
	for _, val := range in {
		if val < min {
			min = val
		}
	}
	return min
}

func Stdev(in []float64) float64 {
	if len(in) == 0 {
		panic("stdev() called in aggregator with 0 terms")
	}
	// Get the average (or mean) of the series
	mean := Avg(in)

	// Calculate the variance
	variance := float64(0)
	for _, term := range in {
		variance += math.Pow((float64(term) - mean), float64(2))
	}
	variance /= float64(len(in))

	// Calculate the standard deviation
	return math.Sqrt(variance)
}

func Sum(in []float64) float64 {
	sum := float64(0)
	for _, term := range in {
		sum += term
	}
	return sum
}

var Funcs = map[string]Func{
	"avg":   Avg,
	"delta": Delta,
	"last":  Last,
	"max":   Max,
	"min":   Min,
	"stdev": Stdev,
	"sum":   Sum,
}

type Aggregator struct {
	Fun          string `json:"fun"`
	fn           Func
	in           chan [][]byte  `json:"-"` // incoming metrics, already split in 3 fields
	out          chan []byte    // outgoing metrics
	Regex        string         `json:"regex,omitempty"`
	regex        *regexp.Regexp // compiled version of Regex
	prefix       []byte         // automatically generated based on regex, for fast preMatch
	OutFmt       string
	outFmt       []byte
	Interval     uint             // expected interval between values in seconds, we will quantize to make sure alginment to interval-spaced timestamps
	Wait         uint             // seconds to wait after quantized time value before flushing final outcome and ignoring future values that are sent too late.
	aggregations []aggregation    // aggregations in process: one for each quantized timestamp and output key, i.e. for each output metric.
	snapReq      chan bool        // chan to issue snapshot requests on
	snapResp     chan *Aggregator // chan on which snapshot response gets sent
	shutdown     chan struct{}    // chan used internally to shut down
	wg           sync.WaitGroup   // tracks worker running state
	now          func() time.Time // returns current time. wraps time.Now except in some unit tests
	tick         <-chan time.Time // controls when to flush
}

// regexToPrefix inspects the regex and returns the longest static prefix part of the regex
// all inputs for which the regex match, must have this prefix
func regexToPrefix(regex string) []byte {
	substr := ""
	for i := 0; i < len(regex); i++ {
		ch := regex[i]
		if i == 0 {
			if ch == '^' {
				continue // good we need this
			} else {
				break // can't deduce any substring here
			}
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			substr += string(ch)
			// "\." means a dot character
		} else if ch == 92 && i+1 < len(regex) && regex[i+1] == '.' {
			substr += "."
			i += 1
		} else {
			//fmt.Println("don't know what to do with", string(ch))
			// anything more advanced should be regex syntax that is more permissive and hence not a static substring.
			break
		}
	}
	return []byte(substr)
}

// New creates an aggregator
func New(fun, regex, outFmt string, interval, wait uint, out chan []byte) (*Aggregator, error) {
	return NewMocked(fun, regex, outFmt, interval, wait, out, 2000, time.Now, clock.AlignedTick(time.Duration(interval)*time.Second))
}

func NewMocked(fun, regex, outFmt string, interval, wait uint, out chan []byte, inBuf int, now func() time.Time, tick <-chan time.Time) (*Aggregator, error) {
	regexObj, err := regexp.Compile(regex)
	if err != nil {
		return nil, err
	}
	fn, ok := Funcs[fun]
	if !ok {
		return nil, fmt.Errorf("no such aggregation function '%s'", fun)
	}
	a := &Aggregator{
		fun,
		fn,
		make(chan [][]byte, inBuf),
		out,
		regex,
		regexObj,
		regexToPrefix(regex),
		outFmt,
		[]byte(outFmt),
		interval,
		wait,
		make([]aggregation, 0, 4),
		make(chan bool),
		make(chan *Aggregator),
		make(chan struct{}),
		sync.WaitGroup{},
		now,
		tick,
	}
	a.wg.Add(1)
	go a.run()
	return a, nil
}

type aggregation struct {
	key    string
	ts     uint
	values []float64
}

func (a *Aggregator) AddOrCreate(key string, ts uint, value float64) {
	for i, agg := range a.aggregations {
		if agg.key == key && agg.ts == ts {
			a.aggregations[i].values = append(agg.values, value)
			return
		}
	}
	if ts > uint(a.now().Unix())-a.Wait {
		a.aggregations = append(a.aggregations, aggregation{key, ts, []float64{value}})
	}
}

// Flush finalizes and removes aggregations that are due
func (a *Aggregator) Flush(ts uint) {
	aggregations2 := make([]aggregation, 0, len(a.aggregations))
	for _, agg := range a.aggregations {
		if agg.ts < ts {
			result := a.fn(agg.values)
			metric := fmt.Sprintf("%s %f %d", string(agg.key), result, agg.ts)
			//log.Debug("aggregator %s-%v-%v values %v -> result %q", a.Fun, a.Regex, a.OutFmt, agg.values, metric)
			a.out <- []byte(metric)
		} else {
			aggregations2 = append(aggregations2, agg)
		}
	}
	a.aggregations = aggregations2
}

func (a *Aggregator) Shutdown() {
	close(a.shutdown)
	a.wg.Wait()
}

func (a *Aggregator) AddMaybe(buf [][]byte) {
	if a.PreMatch(buf[0]) {
		a.in <- buf
	}
}

//PreMatch checks if the specified metric might match the regex
//by comparing it to the prefix derived from the regex
//if this returns false, the metric will definitely not match the regex and be ignored.
func (a *Aggregator) PreMatch(buf []byte) bool {
	return bytes.HasPrefix(buf, a.prefix)
}

func (a *Aggregator) run() {
	for {
		select {
		case fields := <-a.in:
			// note, we rely here on the fact that the packet has already been validated
			key := fields[0]

			matches := a.regex.FindSubmatchIndex(key)
			if len(matches) == 0 {
				continue
			}
			value, _ := strconv.ParseFloat(string(fields[1]), 64)
			t, _ := strconv.ParseUint(string(fields[2]), 10, 0)
			ts := uint(t)

			var dst []byte
			outKey := string(a.regex.Expand(dst, a.outFmt, key, matches))
			quantized := ts - (ts % a.Interval)
			a.AddOrCreate(outKey, quantized, value)
		case now := <-a.tick:
			thresh := now.Add(-time.Duration(a.Wait) * time.Second)
			a.Flush(uint(thresh.Unix()))
		case <-a.snapReq:
			var aggs []aggregation
			copy(aggs, a.aggregations)
			s := &Aggregator{
				a.Fun,
				a.fn,
				nil,
				nil,
				a.Regex,
				nil,
				a.prefix,
				a.OutFmt,
				nil,
				a.Interval,
				a.Wait,
				aggs,
				nil,
				nil,
				nil,
				sync.WaitGroup{},
				time.Now,
				nil,
			}

			a.snapResp <- s
		case <-a.shutdown:
			thresh := a.now().Add(-time.Duration(a.Wait) * time.Second)
			a.Flush(uint(thresh.Unix()))
			a.wg.Done()
			return

		}
	}
}

// to view the state of the aggregator at any point in time
func (a *Aggregator) Snapshot() *Aggregator {
	a.snapReq <- true
	return <-a.snapResp
}
