package service

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/alireza0/s-ui/database"
	"github.com/alireza0/s-ui/database/model"

	"gorm.io/gorm"
)

func clientTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	return database.GetDB()
}

func createClient(t *testing.T, db *gorm.DB, c *model.Client) *model.Client {
	t.Helper()
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Inbounds == nil {
		c.Inbounds = json.RawMessage(`[]`)
	}
	if c.Links == nil {
		c.Links = json.RawMessage(`[]`)
	}
	if err := db.Create(c).Error; err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return c
}

func reload(t *testing.T, db *gorm.DB, id uint) model.Client {
	t.Helper()
	var got model.Client
	if err := db.Model(model.Client{}).Where("id = ?", id).First(&got).Error; err != nil {
		t.Fatalf("reloading client: %v", err)
	}
	return got
}

// A client saved with auto_reset on and reset_days zero matched the periodic
// reset query, then had NextReset set to dt + 0 == dt -- so it matched again on
// the very next run of the deplete job, every minute, forever. Its traffic was
// folded into the totals and zeroed each time, which meant up+down could never
// exceed its volume and a quota-limited account became unlimited.
func TestPeriodicResetSkipsZeroResetDays(t *testing.T) {
	db := clientTestDB(t)
	s := &ClientService{}

	const now = int64(1_000_000)
	c := createClient(t, db, &model.Client{
		Name:      "zero-days",
		Enable:    true,
		AutoReset: true,
		ResetDays: 0,
		NextReset: now - 1, // already due
		Up:        500,
		Down:      700,
	})

	if _, err := s.ResetClients(db, now); err != nil {
		t.Fatalf("ResetClients: %v", err)
	}

	got := reload(t, db, c.Id)
	if got.Up != 500 || got.Down != 700 {
		t.Errorf("traffic was reset for a client with reset_days = 0: up=%d down=%d", got.Up, got.Down)
	}
	if got.TotalUp != 0 || got.TotalDown != 0 {
		t.Errorf("traffic was folded into the totals: total_up=%d total_down=%d", got.TotalUp, got.TotalDown)
	}
}

// The normal path still has to work: a due client with a sane period is reset
// and its next boundary moved forward by exactly that many days.
func TestPeriodicResetRollsTrafficForward(t *testing.T) {
	db := clientTestDB(t)
	s := &ClientService{}

	const now = int64(1_000_000)
	c := createClient(t, db, &model.Client{
		Name:      "monthly",
		Enable:    true,
		AutoReset: true,
		ResetDays: 30,
		NextReset: now - 1,
		Up:        500,
		Down:      700,
		TotalUp:   100,
		TotalDown: 200,
	})

	if _, err := s.ResetClients(db, now); err != nil {
		t.Fatalf("ResetClients: %v", err)
	}

	got := reload(t, db, c.Id)
	if got.Up != 0 || got.Down != 0 {
		t.Errorf("period traffic was not cleared: up=%d down=%d", got.Up, got.Down)
	}
	if got.TotalUp != 600 || got.TotalDown != 900 {
		t.Errorf("totals = %d/%d, want 600/900", got.TotalUp, got.TotalDown)
	}
	if want := now + 30*86400; got.NextReset != want {
		t.Errorf("next_reset = %d, want %d", got.NextReset, want)
	}
}

// A client whose boundary is still in the future must be left alone.
func TestPeriodicResetLeavesFutureBoundariesAlone(t *testing.T) {
	db := clientTestDB(t)
	s := &ClientService{}

	const now = int64(1_000_000)
	c := createClient(t, db, &model.Client{
		Name:      "not-due",
		Enable:    true,
		AutoReset: true,
		ResetDays: 30,
		NextReset: now + 86400,
		Up:        500,
	})

	if _, err := s.ResetClients(db, now); err != nil {
		t.Fatalf("ResetClients: %v", err)
	}

	if got := reload(t, db, c.Id); got.Up != 500 {
		t.Errorf("a client that was not due had its traffic reset: up=%d", got.Up)
	}
}

// preserveServerOwnedFields has to restore the traffic counters as well as the
// timestamps. The stats job adds to up/down every ten seconds, so an admin who
// opened the client editor and saved a minute later used to write the stale
// values back and silently discard that minute of usage.
func TestEditDoesNotRollBackTraffic(t *testing.T) {
	db := clientTestDB(t)
	s := &ClientService{}

	c := createClient(t, db, &model.Client{
		Name:   "someone",
		Enable: true,
		Up:     100,
		Down:   200,
	})

	// The stats job lands while the editor is open.
	if err := db.Model(model.Client{}).Where("id = ?", c.Id).
		Updates(map[string]any{"up": 1500, "down": 2500}).Error; err != nil {
		t.Fatal(err)
	}

	// The form posts back what it was rendered with.
	stale := &model.Client{
		Id:     c.Id,
		Name:   "someone",
		Enable: true,
		Up:     100,
		Down:   200,
		Desc:   "edited",
	}
	s.preserveServerOwnedFields(db, stale)

	if stale.Up != 1500 || stale.Down != 2500 {
		t.Errorf("stale form values were not replaced: up=%d down=%d, want 1500/2500", stale.Up, stale.Down)
	}
}

// The changes log stores the client name as JSON. It was built by string
// concatenation, so a name containing a quote or a backslash produced a row no
// reader could parse.
func TestClientNameJSONIsValid(t *testing.T) {
	for _, name := range []string{
		`plain`,
		`he"llo`,
		`back\slash`,
		`both"and\`,
		"new\nline",
		`emoji 🎈`,
	} {
		raw := clientNameJSON(name)
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Errorf("name %q produced invalid JSON %s: %v", name, raw, err)
			continue
		}
		if decoded != name {
			t.Errorf("round trip of %q gave %q", name, decoded)
		}
	}
}
