package main

import (
	"testing"
	"time"
)

// The rig's whole claim is that it reports a range rather than one number, so
// the two functions that produce the range are the two things here worth a
// test. They are also the only code in this package that can be checked
// without a server, a container and a memory limit.

func TestASpreadNamesTheEndsAndTheMiddleOfWhatWasMeasured(t *testing.T) {
	// Deliberately unsorted, and deliberately with the interesting reading —
	// the slow one — last: a function that reported readings[0] as the min
	// would pass on a sorted input.
	got := spreadOf([]float64{900, 120, 1000, 950})
	if got.Runs != 4 {
		t.Errorf("Runs is %d, want 4", got.Runs)
	}
	if got.Min != 120 {
		t.Errorf("Min is %v, want 120", got.Min)
	}
	if got.Max != 1000 {
		t.Errorf("Max is %v, want 1000", got.Max)
	}
	// Four readings, so the median is the mean of 900 and 950.
	if got.Median != 925 {
		t.Errorf("Median is %v, want 925", got.Median)
	}

	// Odd count: the median is a reading, not a mean of two.
	odd := spreadOf([]float64{3, 1, 2})
	if odd.Median != 2 || odd.Min != 1 || odd.Max != 3 || odd.Runs != 3 {
		t.Errorf("odd spread is %+v, want min 1 median 2 max 3 over 3 runs", odd)
	}
}

// A spread of nothing must be a zero spread with Runs 0, not a zero spread
// that reads like three measurements of zero. Runs is what tells them apart,
// which is why it is in the struct.
func TestASpreadOfNothingSaysItMeasuredNothing(t *testing.T) {
	got := spreadOf(nil)
	if got.Runs != 0 {
		t.Errorf("Runs is %d, want 0", got.Runs)
	}
}

// spreadOf must not reorder what it is handed. The caller keeps its readings
// in the order the runs happened, and a sort in place would silently destroy
// that for whoever looks at them next.
func TestASpreadLeavesTheCallersReadingsAlone(t *testing.T) {
	readings := []float64{3, 1, 2}
	spreadOf(readings)
	if readings[0] != 3 || readings[1] != 1 || readings[2] != 2 {
		t.Errorf("the readings were reordered to %v", readings)
	}
}

func TestALatencyReportsANearestRankP99AndTheTrueMax(t *testing.T) {
	// 100 readings: ninety-nine of 1ms and one of 500ms. The p99 at nearest
	// rank is the 99th, which is 1ms, and the max is the outlier. A p99 that
	// came back 500 would mean the tail had swallowed the body.
	durations := make([]time.Duration, 0, 100)
	for i := 0; i < 99; i++ {
		durations = append(durations, time.Millisecond)
	}
	durations = append(durations, 500*time.Millisecond)

	got := latencyOf(durations)
	if got.Count != 100 {
		t.Errorf("Count is %d, want 100", got.Count)
	}
	if got.MinMS != 1 || got.MedianMS != 1 {
		t.Errorf("min/median are %v/%v, want 1/1", got.MinMS, got.MedianMS)
	}
	if got.P99MS != 1 {
		t.Errorf("P99 is %v, want 1 — the outlier is the 100th of 100 readings", got.P99MS)
	}
	if got.MaxMS != 500 {
		t.Errorf("Max is %v, want 500 — the outlier is the whole reason this field exists", got.MaxMS)
	}
}

func TestALatencyOfNothingIsNotAMeasurementOfZero(t *testing.T) {
	got := latencyOf(nil)
	if got.Count != 0 {
		t.Errorf("Count is %d, want 0", got.Count)
	}
}
