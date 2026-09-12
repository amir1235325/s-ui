package service

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/alireza0/s-ui/core"
	"github.com/alireza0/s-ui/database"
)

// lifecycleService gives the test a database and a real but unstarted Core, so
// the lifecycle functions can be called for their locking and guard behaviour
// without ever bringing a sing-box instance up.
func lifecycleService(t *testing.T) *ConfigService {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	prev := corePtr
	corePtr = core.NewCore()
	t.Cleanup(func() { corePtr = prev })
	return &ConfigService{}
}

// mustFinish fails the test rather than hanging the suite when a lifecycle call
// deadlocks, which is the failure mode a mistake in the locking would produce.
func mustFinish(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return: the lifecycle lock is deadlocked", what)
	}
}

// StopCore, RestartCore and SetMaintenance all take the lifecycle lock, and all
// three need to stop the core. If any of them called the public StopCore
// instead of the locked variant it would deadlock on itself, so this walks the
// three entry points that reach a stop.
func TestLifecycleEntryPointsDoNotSelfDeadlock(t *testing.T) {
	s := lifecycleService(t)

	mustFinish(t, "StopCore", func() {
		if err := s.StopCore(); err != nil {
			t.Errorf("StopCore on a core that never started: %v", err)
		}
	})

	// With maintenance on, RestartCore refuses before touching the core, and
	// SetMaintenance(true) finds it already stopped. Both still take and
	// release the lock, which is what is under test.
	if err := s.SettingService.SetMaintenance(true); err != nil {
		t.Fatal(err)
	}
	mustFinish(t, "RestartCore", func() {
		if err := s.RestartCore(); err == nil {
			t.Error("RestartCore succeeded while stopped for maintenance")
		}
	})
	mustFinish(t, "SetMaintenance", func() {
		if err := s.SetMaintenance(true); err != nil {
			t.Errorf("SetMaintenance(true) while already stopped: %v", err)
		}
	})
	mustFinish(t, "StopCore again", func() {
		if err := s.StopCore(); err != nil {
			t.Errorf("second StopCore: %v", err)
		}
	})
}

// The bug this locks down: SetMaintenance used to run while a start was still
// in flight. It read IsRunning(), which is only set once Start returns, saw
// false, stopped nothing and reported success -- leaving the panel showing
// maintenance while the core came up and served clients anyway.
//
// The fix is that a maintenance toggle takes the same lock a start holds for
// its whole sequence, so it cannot observe that half-started state. Holding the
// lock here stands in for the in-flight start.
func TestSetMaintenanceWaitsForAnInFlightStart(t *testing.T) {
	s := lifecycleService(t)

	lifecycleMu.Lock()

	returned := make(chan error, 1)
	go func() { returned <- s.SetMaintenance(true) }()

	select {
	case <-returned:
		lifecycleMu.Unlock()
		t.Fatal("SetMaintenance returned while a start sequence held the lock")
	case <-time.After(100 * time.Millisecond):
		// Still blocked, which is the point.
	}

	lifecycleMu.Unlock()

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("SetMaintenance after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SetMaintenance never returned after the lock was released")
	}

	maintenance, err := s.SettingService.GetMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	if !maintenance {
		t.Error("maintenance was not persisted")
	}
}

// StartCore is called by the five-second watchdog. It must not queue up behind
// a sequence that is already running, or a slow start would leave watchdog
// ticks stacked up waiting to run against a core that is already up.
func TestStartCoreSkipsWhenBusy(t *testing.T) {
	s := lifecycleService(t)

	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()

	done := make(chan error, 1)
	go func() { done <- s.StartCore() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("StartCore should skip quietly while busy, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartCore blocked instead of skipping while another sequence held the lock")
	}
}

// The cooldown stops the five-second watchdog hammering a config that will not
// load. These cover the watchdog side, which is what the cooldown exists for.
//
// The other half -- that RestartCore and leaving maintenance pass
// bypassCooldown, so a recent failure cannot make a button press silently do
// nothing -- is deliberately not tested here: both paths go on to build and
// start a real sing-box instance, which binds ports and trips sing-box's own
// race in route.NetworkManager. That behaviour belongs in an integration test,
// not this package.
func TestCooldownHoldsOffTheWatchdog(t *testing.T) {
	s := lifecycleService(t)

	setFailTime(t, time.Now())

	if !coolingDown() {
		t.Fatal("a start that just failed did not arm the cooldown")
	}

	mustFinish(t, "StartCore during cooldown", func() {
		if err := s.StartCore(); err != nil {
			t.Errorf("StartCore during cooldown: %v", err)
		}
	})
	if corePtr.IsRunning() {
		t.Error("StartCore started the core despite the cooldown")
	}
}

// The cooldown was never cleared on a successful start, so a single early
// failure kept it armed and every later watchdog tick was still comparing
// against that old timestamp.
func TestCooldownClearsOnSuccess(t *testing.T) {
	setFailTime(t, time.Now())
	if !coolingDown() {
		t.Fatal("cooldown not armed")
	}

	// What startCoreLocked does after corePtr.Start returns without error.
	failMu.Lock()
	lastStartFailTime = time.Time{}
	failMu.Unlock()

	if coolingDown() {
		t.Error("cooldown still armed after a successful start cleared it")
	}
}

func TestCooldownExpires(t *testing.T) {
	setFailTime(t, time.Now().Add(-2*startCooldown))
	if coolingDown() {
		t.Errorf("a failure %v ago should be outside the %v cooldown", 2*startCooldown, startCooldown)
	}
}

func setFailTime(t *testing.T, at time.Time) {
	t.Helper()
	failMu.Lock()
	lastStartFailTime = at
	failMu.Unlock()
	t.Cleanup(func() {
		failMu.Lock()
		lastStartFailTime = time.Time{}
		failMu.Unlock()
	})
}
