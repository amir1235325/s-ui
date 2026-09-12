package database

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alireza0/s-ui/cmd/migration"
	"github.com/alireza0/s-ui/config"
	"github.com/alireza0/s-ui/logger"
	"github.com/alireza0/s-ui/util/common"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func GetDb(exclude string) ([]byte, error) {
	excluded := make(map[string]bool)
	for _, name := range strings.Split(exclude, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			excluded[name] = true
		}
	}

	// os.CreateTemp, not a hand-built name. The old path was
	// `dir + config.GetName() + time.Now().Format("20060102-200203")`, which had
	// two defects: no separator, so the file landed in the binary's *parent*
	// directory; and "200203" is a typo for the reference clock "150405", so
	// every backup taken in the same period rendered the same name. Two
	// downloads on one day opened the same file and deleted it from under each
	// other.
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, config.GetName()+"-backup-*.db")
	if err != nil {
		return nil, err
	}
	dbPath := tmp.Name()
	// SQLite opens the path itself; this handle is only here to reserve a
	// unique name.
	tmp.Close()
	defer os.Remove(dbPath)

	backupDb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	backupClosed := false
	closeBackup := func() {
		if backupClosed {
			return
		}
		backupClosed = true
		if sqlDB, e := backupDb.DB(); e == nil {
			_ = sqlDB.Close()
		}
	}
	defer closeBackup()

	// Same list InitDB migrates, so a table can never be created in the live
	// database but missing from the backup.
	if err = backupDb.AutoMigrate(schemaModels()...); err != nil {
		return nil, err
	}

	for _, t := range schema() {
		if excluded[t.name] {
			continue
		}
		if err := t.copyRows(db, backupDb); err != nil {
			return nil, common.NewErrorf("backing up %s: %v", t.name, err)
		}
	}

	// Fold the WAL back into the main file, otherwise the bytes read below are
	// missing everything still sitting in the sidecar.
	if err = backupDb.Exec("PRAGMA wal_checkpoint(TRUNCATE);").Error; err != nil {
		return nil, err
	}
	closeBackup()

	return os.ReadFile(dbPath)
}

func ImportDB(file multipart.File) error {
	// Check if the file is a SQLite database
	isValidDb, err := IsSQLiteDB(file)
	if err != nil {
		return common.NewErrorf("Error checking db file format: %v", err)
	}
	if !isValidDb {
		return common.NewError("Invalid db file format")
	}

	// Reset the file reader to the beginning
	if _, err = file.Seek(0, 0); err != nil {
		return common.NewErrorf("Error resetting file reader: %v", err)
	}

	dbPath := config.GetDBPath()
	tempPath := fmt.Sprintf("%s.temp", dbPath)
	if err = os.RemoveAll(tempPath); err != nil {
		return common.NewErrorf("Error removing existing temporary db file: %v", err)
	}

	// Everything below up to the rename works on the upload alone. The live
	// connection pool stays open throughout: it used to be closed here, before
	// the copy and the validation, so any failure after that point -- a
	// truncated upload, a full disk -- left every later query in the process
	// failing with "sql: database is closed" until someone restarted the
	// service by hand.
	tempFile, err := os.Create(tempPath)
	if err != nil {
		return common.NewErrorf("Error creating temporary db file: %v", err)
	}
	_, err = io.Copy(tempFile, file)
	tempFile.Close()
	if err != nil {
		os.Remove(tempPath)
		return common.NewErrorf("Error saving db: %v", err)
	}
	defer os.Remove(tempPath)

	// Open it and check it is actually an s-ui database, not merely some
	// SQLite file. The header check above passes for any SQLite file at all --
	// a browser profile, another panel's database -- and importing one of those
	// used to replace the live database and then crash the process on the
	// migration, leaving systemd to restart it straight into the same crash.
	if err = validateImport(tempPath); err != nil {
		return err
	}

	// Close the live pool and fold its WAL back in. Renaming the database out
	// from under a -wal/-shm pair leaves those sidecars beside the imported
	// file, and SQLite then runs recovery for a different database against it.
	if sqlDB, e := db.DB(); e == nil {
		_ = db.Exec("PRAGMA wal_checkpoint(TRUNCATE);").Error
		_ = sqlDB.Close()
	}
	removeSidecars(dbPath)

	// Keep the pre-import database, timestamped, so a bad import is
	// recoverable. The old code deleted it on the success path.
	fallbackPath := fmt.Sprintf("%s.backup-%s", dbPath, time.Now().Format("20060102-150405"))
	if err = os.Rename(dbPath, fallbackPath); err != nil {
		reopen(dbPath)
		return common.NewErrorf("Error backing up current db file: %v", err)
	}

	if err = os.Rename(tempPath, dbPath); err != nil {
		if errRename := os.Rename(fallbackPath, dbPath); errRename != nil {
			return common.NewErrorf("Error moving db file and restoring fallback: %v", errRename)
		}
		reopen(dbPath)
		return common.NewErrorf("Error moving db file: %v", err)
	}

	if err = migration.MigrateDb(); err != nil {
		restoreFallback(dbPath, fallbackPath)
		return common.NewErrorf("Error migrating db: %v", err)
	}
	if err = InitDB(dbPath); err != nil {
		restoreFallback(dbPath, fallbackPath)
		return common.NewErrorf("Error initialising imported db: %v", err)
	}

	logger.Info("database imported; previous database kept at ", fallbackPath)

	// Restart app
	if err = SendSighup(); err != nil {
		return common.NewErrorf("Error restarting app: %v", err)
	}

	return nil
}

// validateImport rejects a SQLite file that is not an s-ui database, before it
// can replace the live one.
func validateImport(path string) error {
	candidate, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		return common.NewErrorf("Error checking db: %v", err)
	}
	defer func() {
		if sqlDB, e := candidate.DB(); e == nil {
			_ = sqlDB.Close()
		}
	}()

	// These three carry the panel's identity: settings holds the schema
	// version and the session secret, and an import without clients and
	// inbounds is not a panel backup whatever else it contains.
	for _, required := range []string{"settings", "clients", "inbounds"} {
		if !candidate.Migrator().HasTable(required) {
			return common.NewErrorf("Not an s-ui database: table %q is missing", required)
		}
	}
	return nil
}

// removeSidecars drops the -wal and -shm files belonging to path. They describe
// the database being replaced, and leaving them next to a different one invites
// SQLite to recover pages into it.
func removeSidecars(path string) {
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			logger.Warning("unable to remove ", path+suffix, ": ", err)
		}
	}
}

// reopen restores the connection pool after an import gave up, so the panel
// keeps serving instead of failing every query until it is restarted.
func reopen(path string) {
	if err := InitDB(path); err != nil {
		logger.Error("unable to reopen the database after a failed import: ", err)
	}
}

func restoreFallback(dbPath, fallbackPath string) {
	removeSidecars(dbPath)
	if err := os.Rename(fallbackPath, dbPath); err != nil {
		logger.Error("unable to restore the database after a failed import: ", err)
		return
	}
	reopen(dbPath)
}

func IsSQLiteDB(file io.Reader) (bool, error) {
	signature := []byte("SQLite format 3\x00")
	buf := make([]byte, len(signature))
	// ReadFull, because a single Read may return fewer bytes than asked for and
	// would then compare a partly-filled buffer.
	if _, err := io.ReadFull(file, buf); err != nil {
		return false, err
	}
	return bytes.Equal(buf, signature), nil
}

func SendSighup() error {
	// Get the current process
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}

	// Send SIGHUP to the current process
	go func() {
		time.Sleep(3 * time.Second)
		if runtime.GOOS == "windows" {
			err = process.Kill()
		} else {
			err = process.Signal(syscall.SIGHUP)
		}
		if err != nil {
			logger.Error("send signal SIGHUP failed:", err)
		}
	}()
	return nil
}
