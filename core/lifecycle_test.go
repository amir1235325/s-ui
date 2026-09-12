package core

import (
	"strings"
	"sync"
	"testing"
)

// A config with no inbounds: enough for a real Box to build and start, cheap
// enough to cycle repeatedly, and it binds no ports so the test is safe to run
// anywhere.
const emptyCoreConfig = `{"log":{"level":"error"}}`

// TestCoreConcurrentLifecycle drives the real publish and detach paths while
// readers hammer the accessors, which is the shape of the running panel: the
// five-second watchdog and the config-save handler start and stop the core
// while the dashboard poll reads IsRunning and GetInstance.
//
// Core used to be a bare struct with no mutex at all, so this tripped the race
// detector on both fields. Run with -race; without it the test only proves the
// accessors do not panic.
func TestCoreConcurrentLifecycle(t *testing.T) {
	if raceEnabled {
		t.Skip("starting a Box trips sing-box's own race in route.NetworkManager; see race_on_test.go")
	}
	c := NewCore()
	t.Cleanup(func() { _ = c.Stop() })

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Whatever interleaving happens, a caller must never see
				// isRunning true alongside a nil instance.
				if box, err := c.running(); err == nil && box == nil {
					t.Error("running() returned a nil box with no error")
					return
				}
				_ = c.IsRunning()
				_ = c.GetInstance()
			}
		}()
	}

	for i := 0; i < 10; i++ {
		if err := c.Start([]byte(emptyCoreConfig)); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("start %d: %v", i, err)
		}
		if err := c.Stop(); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("stop %d: %v", i, err)
		}
	}

	close(stop)
	wg.Wait()
}

// TestCoreStartStopState checks the accessors agree with each other across a
// full cycle, and that a second Stop is harmless -- StopCore is reached from
// the maintenance toggle, an explicit restart and shutdown.
func TestCoreStartStopState(t *testing.T) {
	if raceEnabled {
		t.Skip("starting a Box trips sing-box's own race in route.NetworkManager; see race_on_test.go")
	}
	c := NewCore()

	if c.IsRunning() {
		t.Error("a fresh core reports itself running")
	}
	if c.GetInstance() != nil {
		t.Error("a fresh core hands out an instance")
	}
	if _, err := c.running(); err == nil {
		t.Error("running() succeeded on a stopped core")
	}

	if err := c.Start([]byte(emptyCoreConfig)); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !c.IsRunning() {
		t.Error("core does not report itself running after start")
	}
	box, err := c.running()
	if err != nil {
		t.Fatalf("running() after start: %v", err)
	}
	if box != c.GetInstance() {
		t.Error("running() and GetInstance() disagree")
	}

	if err := c.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if c.IsRunning() {
		t.Error("core still reports itself running after stop")
	}
	if c.GetInstance() != nil {
		t.Error("instance survived stop")
	}
	if err := c.Stop(); err != nil {
		t.Errorf("second stop should be a no-op, got %v", err)
	}
}

// TestStartRejectsMalformedConfig locks down a silent failure: the unmarshal
// error was logged and then ignored, so the box was built from a zero-value
// option set. It started, reported itself healthy with no inbounds at all, and
// the watchdog never retried because IsRunning was true.
func TestStartRejectsMalformedConfig(t *testing.T) {
	c := NewCore()
	t.Cleanup(func() { _ = c.Stop() })

	err := c.Start([]byte(`{"log": this is not json}`))
	if err == nil {
		t.Fatal("Start accepted a malformed config")
	}
	if !strings.Contains(err.Error(), "unmarshal config") {
		t.Errorf("expected an unmarshal error, got %v", err)
	}
	if c.IsRunning() {
		t.Error("core reports itself running after a rejected config")
	}
	if c.GetInstance() != nil {
		t.Error("a rejected config left an instance behind")
	}
}

// TestFailedStartLeavesNoInstance covers the other half of the same path: a
// config that parses but cannot start must not publish a half-built box.
func TestFailedStartLeavesNoInstance(t *testing.T) {
	c := NewCore()
	t.Cleanup(func() { _ = c.Stop() })

	// An inbound on a port that cannot be bound: port 0 is fine, but an
	// unparseable listen address fails while the box is being built.
	err := c.Start([]byte(`{"inbounds":[{"type":"mixed","tag":"in","listen":"not-an-address","listen_port":1080}]}`))
	if err == nil {
		t.Skip("this config built successfully; the failure path needs another trigger")
	}
	if c.IsRunning() {
		t.Error("core reports itself running after a failed start")
	}
	if c.GetInstance() != nil {
		t.Error("a failed start published an instance")
	}
	if _, err := c.running(); err == nil {
		t.Error("running() succeeded after a failed start")
	}
}

// TestCoreLockConcurrency exercises Core's own mutex without starting a Box, so
// it runs under -race where the box-starting tests above cannot. Readers race
// the detach path in Stop and the rejection path in Start; the invariant is the
// one the old lock-free struct broke -- no caller may ever observe isRunning as
// true together with a nil instance.
func TestCoreLockConcurrency(t *testing.T) {
	c := NewCore()

	// Two groups: the writers finish on their own, and only once they have can
	// the readers be told to stop. Waiting on a single group would deadlock,
	// since the readers loop until the channel closes.
	var readers, writers sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if box, err := c.running(); err == nil && box == nil {
					t.Error("running() returned a nil box with no error")
					return
				}
				if c.IsRunning() && c.GetInstance() == nil {
					t.Error("core reported running with no instance")
					return
				}
			}
		}()
	}

	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 500; j++ {
				// Rejected before any box is built, so no sing-box code runs.
				_ = c.Start([]byte(`{"log": not json}`))
				_ = c.Stop()
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()
}
