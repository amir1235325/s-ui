package migration

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/alireza0/s-ui/config"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// MigrateDb brings an older database up to the current version.
//
// It returns an error rather than calling log.Fatal, which it used to do from
// six places. That mattered because ImportDB calls this in the middle of a
// restore: a migration failure killed the whole panel process with the live
// database already renamed away, and systemd restarted it straight into the
// same failure.
func MigrateDb() error {
	// void running on first install
	path := config.GetDBPath()
	if _, err := os.Stat(path); err != nil {
		fmt.Println("Database not found")
		return nil
	}

	db, err := gorm.Open(sqlite.Open(path))
	if err != nil {
		return err
	}
	defer func() {
		if sqlDB, e := db.DB(); e == nil {
			_ = sqlDB.Close()
		}
	}()

	tx := db.Begin()
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	currentVersion := config.GetVersion()
	dbVersion := ""
	// The error was discarded here, so a missing or unreadable settings table
	// was indistinguishable from an unset version -- and the empty string sends
	// the whole legacy chain through again.
	if err := tx.Raw("SELECT value FROM settings WHERE key = ?", "version").Find(&dbVersion).Error; err != nil {
		return fmt.Errorf("reading database version: %w", err)
	}
	fmt.Println("Current version:", currentVersion, "\nDatabase version:", dbVersion)

	if currentVersion == dbVersion {
		fmt.Println("Database is up to date, no need to migrate")
		return nil
	}

	fmt.Println("Start migrating database...")

	// Before 1.2
	if dbVersion == "" {
		if err := to1_1(tx); err != nil {
			return fmt.Errorf("migration to 1.1 failed: %w", err)
		}
		if err := to1_2(tx); err != nil {
			return fmt.Errorf("migration to 1.2 failed: %w", err)
		}
		dbVersion = "1.2"
	}

	// Before 1.3. Was `dbVersion[0:3] == "1.2"`, which panics on any stored
	// value shorter than three characters.
	if major, minor := majorMinor(dbVersion); major == 1 && minor == 2 {
		if err := to1_3(tx); err != nil {
			return fmt.Errorf("migration to 1.3 failed: %w", err)
		}
	}

	// Before 1.5.1: back-fill self-signed TLS public-key pins and rewrite OutJson
	if compareVersions(dbVersion, "1.5.1") < 0 {
		if err := to1_5_1(tx); err != nil {
			return fmt.Errorf("migration to 1.5.1 failed: %w", err)
		}
	}

	if compareVersions(dbVersion, "1.5.2") < 0 {
		if err := to1_5_2(tx); err != nil {
			return fmt.Errorf("migration to 1.5.2 failed: %w", err)
		}
	}

	if err := setVersion(tx, currentVersion); err != nil {
		return fmt.Errorf("update version failed: %w", err)
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("committing migration: %w", err)
	}
	committed = true
	fmt.Println("Migration done!")
	return nil
}

// setVersion records the schema version, inserting the row when it is absent.
//
// A plain UPDATE affects zero rows and reports no error when there is no
// version row -- and ResetSettings deletes every settings row, so after
// `s-ui setting -reset` the next migrate replayed the entire legacy chain
// against a current database, failed to record anything, and did it again on
// every subsequent run.
func setVersion(tx *gorm.DB, version string) error {
	res := tx.Exec("UPDATE settings SET value = ? WHERE key = ?", version, "version")
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return tx.Exec("INSERT INTO settings (key, value) VALUES (?, ?)", "version", version).Error
}

// compareVersions orders two dotted versions numerically, returning -1, 0 or 1.
//
// These were compared as strings, which is correct only while every component
// is a single digit: "1.10.0" < "1.5.1" is true lexicographically, so the
// release after 1.9 would have replayed to1_5_1 against every database and
// stripped the explicit CA from every TLS client config.
//
// A missing component counts as zero, so "1.2" and "1.2.0" compare equal.
func compareVersions(a, b string) int {
	av, bv := parseVersion(a), parseVersion(b)
	for i := range av {
		switch {
		case av[i] < bv[i]:
			return -1
		case av[i] > bv[i]:
			return 1
		}
	}
	return 0
}

func majorMinor(v string) (int, int) {
	parsed := parseVersion(v)
	return parsed[0], parsed[1]
}

// parseVersion reads up to three numeric components. Anything it cannot parse
// stops the scan and leaves the rest at zero, so a garbage value sorts as the
// oldest possible version rather than panicking or comparing as text.
func parseVersion(v string) [3]int {
	var parsed [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return parsed
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		n, err := strconv.Atoi(part)
		if err != nil {
			return parsed
		}
		parsed[i] = n
	}
	return parsed
}
