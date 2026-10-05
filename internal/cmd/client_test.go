package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/dittofleet/crosstalk/internal/config"
)

var quick = patience{retry: time.Millisecond, giveUp: 50 * time.Millisecond}

// runs plays back results, one per attempt, and the error each run gave
// after the last.
func runs(results ...error) (func() error, *int) {
	n := 0
	return func() error {
		n++
		if n > len(results) {
			return results[len(results)-1]
		}
		return results[n-1]
	}, &n
}

var notRunning = errors.New("the crosstalk daemon is not running")

func TestStayConnectedCarriesOnAcrossARestart(t *testing.T) {
	// Gone, not back yet, briefly refused its own name, then gone again
	// and still there when the machine leaves the hub.
	taken := &Failure{Code: "name-taken"}
	once, n := runs(errDaemonGone, notRunning, taken, errDaemonGone, config.ErrNotJoined)
	if err := stayConnected(quick, once); !errors.Is(err, config.ErrNotJoined) || *n != 5 {
		t.Fatalf("ended with %v after %d runs", err, *n)
	}
}

func TestStayConnectedGivesUpOnADaemonThatStaysGone(t *testing.T) {
	once, n := runs(errDaemonGone, notRunning)
	start := time.Now()
	err := stayConnected(quick, once)
	if !errors.Is(err, notRunning) || time.Since(start) < quick.giveUp || *n < 3 {
		t.Fatalf("ended with %v after %d runs and %s", err, *n, time.Since(start))
	}
}

func TestStayConnectedStopsOnTroubleAtTheStart(t *testing.T) {
	for _, first := range []error{notRunning, &Failure{Code: "name-taken"}} {
		once, n := runs(first)
		if err := stayConnected(quick, once); err != first || *n != 1 {
			t.Errorf("ended with %v after %d runs", err, *n)
		}
	}
}
