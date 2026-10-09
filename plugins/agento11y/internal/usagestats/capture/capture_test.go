package capture

import (
	"sync"
	"testing"
	"time"
)

// TestZeroValueRecordsNothing is the important one: the zero value is what a
// suppressed or never-dispatched invocation reads, so a meaningful default
// would make it report a confident, wrong event.
func TestZeroValueRecordsNothing(t *testing.T) {
	Reset()
	got := Snapshot()

	if got != (Invocation{}) {
		t.Fatalf("Snapshot() after Reset() = %+v, want the zero value", got)
	}
	if got.Completed {
		t.Error("Completed defaults to true; a panicking invocation would then report success")
	}
	if got.Suppressed {
		t.Error("Suppressed defaults to true; nothing would ever be reported")
	}
	if got.Launched {
		t.Error("Launched defaults to true; every invocation would report as a launcher handoff")
	}
	if got.Command != "" || got.Surface != "" || got.Agent != "" || got.Skill != "" || got.Flags != "" {
		t.Errorf("a vocabulary field has a non-empty default: %+v", got)
	}
	if got.ExitCode != 0 {
		t.Errorf("ExitCode defaults to %d", got.ExitCode)
	}
}

func TestSettersRecordWhatTheyAreGiven(t *testing.T) {
	Reset()
	start := time.Now()

	SetStart(start)
	SetDispatch("command", "skills show", "", "json")
	SetSkill("setup-coding-agent")
	SetExit(2)
	SetCompleted()
	SetLaunched()

	got := Snapshot()
	if !got.Start.Equal(start) {
		t.Errorf("Start = %v, want %v", got.Start, start)
	}
	if got.Surface != "command" || got.Command != "skills show" || got.Flags != "json" {
		t.Errorf("dispatch not recorded: %+v", got)
	}
	if got.Skill != "setup-coding-agent" {
		t.Errorf("Skill = %q", got.Skill)
	}
	if got.ExitCode != 2 || !got.Completed || !got.Launched {
		t.Errorf("terminal state not recorded: %+v", got)
	}
}

// TestSuppressIsOneWay: hook dispatch suppresses early, and a later branch
// must not re-enable reporting.
func TestSuppressIsOneWay(t *testing.T) {
	Reset()
	Suppress()
	SetDispatch("command", "doctor", "", "")
	SetCompleted()

	if !Snapshot().Suppressed {
		t.Error("Suppressed was cleared by a later write")
	}
}

func TestDurationIsZeroWithoutAStart(t *testing.T) {
	Reset()

	// Without the guard this measures from the zero time: ~2000 years.
	if got := Snapshot().Duration(time.Now()); got != 0 {
		t.Errorf("Duration() = %v with no recorded start, want 0", got)
	}
}

func TestDurationMeasuresFromStart(t *testing.T) {
	Reset()
	start := time.Now()
	SetStart(start)

	if got := Snapshot().Duration(start.Add(1500 * time.Millisecond)); got != 1500*time.Millisecond {
		t.Errorf("Duration() = %v, want 1.5s", got)
	}
}

// TestConcurrentAccessIsSafe: the process starts goroutines, so the race
// detector is what makes this worth its runtime.
func TestConcurrentAccessIsSafe(t *testing.T) {
	Reset()

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			SetExit(i)
			SetDispatch("command", "doctor", "", "")
			SetCompleted()
			_ = Snapshot()
		}(i)
	}
	wg.Wait()

	if !Snapshot().Completed {
		t.Error("Completed was not recorded")
	}
}
