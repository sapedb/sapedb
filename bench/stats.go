package main

import (
	"math"
	"sort"
	"time"
)

// Spread is what this rig reports instead of an average.
//
// An average of five runs hides the one that took four times as long, and the
// slow run is the interesting one: it is the page cache missing, the fsync
// queue filling, or the container about to run out of memory. Min, median and
// max say where the runs actually landed, and Runs says how many readings the
// three numbers came from — three numbers derived from two readings are not a
// spread, they are a coincidence, and a reader has to be able to tell.
type Spread struct {
	Runs   int     `json:"runs"`
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

// spreadOf sorts a copy of the readings and reads the three points off it.
//
// A median of an even number of readings is the mean of the middle two, which
// is the ordinary convention and the only point in this file where a number
// that was never measured is reported. It is called a median rather than a
// reading for that reason.
func spreadOf(readings []float64) Spread {
	if len(readings) == 0 {
		return Spread{}
	}
	sorted := append([]float64(nil), readings...)
	sort.Float64s(sorted)

	middle := len(sorted) / 2
	median := sorted[middle]
	if len(sorted)%2 == 0 {
		median = (sorted[middle-1] + sorted[middle]) / 2
	}
	return Spread{
		Runs:   len(sorted),
		Min:    sorted[0],
		Median: median,
		Max:    sorted[len(sorted)-1],
	}
}

// Latency is the spread of one request's round trip, in milliseconds, with the
// tail called out.
//
// Throughput and latency are not the same measurement and this rig reports
// both: a rate of 900 rows/second says nothing about whether one row in a
// hundred waited a second, and on a machine with a memory limit that one row
// is the whole story.
type Latency struct {
	Count       int     `json:"count"`
	MinMS       float64 `json:"minMs"`
	MedianMS    float64 `json:"medianMs"`
	P99MS       float64 `json:"p99Ms"`
	MaxMS       float64 `json:"maxMs"`
	MeasuredAll bool    `json:"measuredAll"`
}

// latencyOf reads the spread of a whole phase's round trips.
//
// MeasuredAll is true when every request of the phase is in `durations`. This
// rig keeps them all — a run is tens of thousands of requests, and a duration
// is eight bytes — but the field is here so a reader of the JSON never has to
// guess whether a p99 was computed over a sample.
func latencyOf(durations []time.Duration) Latency {
	if len(durations) == 0 {
		return Latency{MeasuredAll: true}
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

	middle := len(sorted) / 2
	median := ms(sorted[middle])
	if len(sorted)%2 == 0 {
		median = (ms(sorted[middle-1]) + ms(sorted[middle])) / 2
	}

	// Nearest-rank p99: the smallest reading at or above the 99th percentile
	// position. No interpolation, so the number printed is one that was
	// actually measured.
	rank := int(math.Ceil(0.99*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}

	return Latency{
		Count:       len(sorted),
		MinMS:       ms(sorted[0]),
		MedianMS:    median,
		P99MS:       ms(sorted[rank]),
		MaxMS:       ms(sorted[len(sorted)-1]),
		MeasuredAll: true,
	}
}
