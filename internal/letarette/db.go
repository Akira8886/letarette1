package letarette

//go:generate go-bindata -pkg $GOPACKAGE -o migrations.go migrations/

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/golang-migrate/migrate/v4"
	sqlite3_migrate "github.com/golang-migrate/migrate/v4/database/sqlite3"
	bindata "github.com/golang-migrate/migrate/v4/source/go_bindata"

	"github.com/erkkah/letarette/internal/auxilliary"
	"github.com/erkkah/letarette/internal/snowball"
	"github.com/erkkah/letarette/pkg/logger"
	"github.com/erkkah/letarette/pkg/protocol"
)

// InterestState represents the state of an interest
type InterestState int

const (
	pending InterestState = iota
	requested
	served
)

// Interest represents one row in the interest list
type Interest struct {
	DocID protocol.DocumentID `db:"docID"`
	State InterestState
}

// InterestListState keeps track of where the index process is
type InterestListState struct {
	CreatedAt        int64               `db:"listCreatedAtNanos"`
	LastUpdated      int64               `db:"lastUpdatedAtNanos"`
	LastUpdatedDocID protocol.DocumentID `db:"lastUpdatedDocID"`
}

func (state InterestListState) lastUpdatedTime() time.Time {
	return time.Unix(0, state.LastUpdated)
}

func (state InterestListState) createdAtTime() time.Time {
	return time.Unix(0, state.CreatedAt)
}

// Database is a live connection to a SQLite database file,
// providing access methods for all db interactions.
type Database interface {
	Close() error
	RawQuery(string) ([]string, error)

	addDocumentUpdates(ctx context.Context, space string, doc []protocol.Document) error
	commitInterestList(ctx context.Context, space string) error
	getLastUpdateTime(context.Context, string) (time.Time, error)

	clearInterestList(context.Context, string) error
	resetRequested(context.Context, string) error

	setInterestList(context.Context, string, []protocol.DocumentID) error
	getInterestList(context.Context, string) ([]Interest, error)
	setInterestState(context.Context, string, protocol.DocumentID, InterestState) error

	getInterestListState(context.Context, string) (InterestListState, error)

	search(ctx context.Context, phrase string, spaces []string, limit uint16, offset uint16) ([]protocol.SearchResult, error)

	getStemmerState() (snowball.Settings, time.Time, error)
	setStemmerState(snowball.Settings) error

	getRawDB() *sqlx.DB
}

type database struct {
	rdb *sqlx.DB
	wdb *sqlx.DB
}

// OpenDatabase connects to a new or existing database and
// migrates the database up to the latest version.
func OpenDatabase(cfg Config) (Database, error) {
	registerCustomDriver(cfg)
	rdb, wdb, err := openDatabase(cfg.Db.Path, cfg.Index.Spaces)
	if err != nil {
		return nil, err
	}

	newDB := &database{rdb, wdb}
	return newDB, nil
}

// ResetMigration forces the migration version of a db.
// It is typically used to back out of a failed migration.
// Note: no migration steps are actually performed, it only
// sets the version and resets the dirty flag.
func ResetMigration(cfg Config, version int) error {
	registerCustomDriver(cfg)
	db, err := openMigrationConnection(cfg.Db.Path)
	if err != nil {
		return err
	}
	var current int
	err = db.Get(&current, "select version from schema_migrations")
	if err != nil {
		return err
	}
	if current < version {
		return fmt.Errorf("Cannot reset migration forward from %v to %v", current, version)
	}
	_, err = db.Exec(`update schema_migrations set version=?, dirty="false"`, version)
	return err
}

func (db *database) Close() error {
	logger.Debug.Printf("Closing database")
	rErr := db.rdb.Close()

	_, tErr := db.wdb.Exec("pragma wal_checkpoint(TRUNCATE);")

	wErr := db.wdb.Close()
	if rErr != nil || tErr != nil || wErr != nil {
		return fmt.Errorf("Failed to close db: %w, %w, %w", rErr, tErr, wErr)
	}
	return nil
}

func (db *database) RawQuery(statement string) ([]string, error) {
	res, err := db.rdb.Queryx(statement)
	if err != nil {
		return nil, err
	}
	result := []string{}
	for res.Next() {
		row, err := res.SliceScan()
		if err != nil {
			return nil, err
		}
		colTypes, _ := res.ColumnTypes()
		var rowdata []string
		for i, col := range row {
			var coldata string
			switch colTypes[i].ScanType().Kind() {
			case reflect.String:
				coldata = fmt.Sprintf("%s", col)
			default:
				coldata = fmt.Sprintf("%v", col)
			}
			rowdata = append(rowdata, coldata)
		}
		result = append(result, strings.Join(rowdata, ", "))
	}
	return result, nil
}

func (db *database) getRawDB() *sqlx.DB {
	return db.wdb
}

func initDB(db *sqlx.DB, sqliteURL string, spaces []string) error {
	migrations, err := AssetDir("migrations")
	if err != nil {
		return err
	}
	res := bindata.Resource(migrations, func(name string) ([]byte, error) {
		return Asset("migrations/" + name)
	})

	sourceDriver, err := bindata.WithInstance(res)
	if err != nil {
		return err
	}

	dbDriver, err := sqlite3_migrate.WithInstance(db.DB, &sqlite3_migrate.Config{})
	if err != nil {
		return err
	}

	m, err := migrate.NewWithInstance("go-bindata", sourceDriver, "letarette", dbDriver)
	if err != nil {
		return err
	}

	version, _, err := m.Version()
	if err != nil && err != migrate.ErrNilVersion {
		return err
	}

	runMigration := version == 0

	if !runMigration {
		next, err := sourceDriver.Next(version)
		if err == nil {
			runMigration = next > version
		} else {
			// The source driver should return ErrNotExist
			_, isPathError := err.(*os.PathError)

			if !isPathError && err != os.ErrNotExist {
				return err
			}
		}
	}

	if runMigration {
		logger.Info.Printf("Applying migrations")
		err = m.Up()
		if err != nil && err != migrate.ErrNoChange {
			return err
		}
	}

	for _, space := range spaces {
		createSpace := `insert into spaces (space, lastUpdatedAtNanos) values(?, 0) on conflict do nothing`
		_, err := db.Exec(createSpace, space)
		if err != nil {
			return fmt.Errorf("Failed to create space table: %w", err)
		}
	}

	return nil
}

const driver = "sqlite3_letarette"

func registerCustomDriver(cfg Config) {
	drivers := sql.Drivers()
	if sort.Search(len(drivers), func(i int) bool { return drivers[i] == driver }) == len(drivers) {
		logger.Debug.Printf("Registering %q driver", driver)
		sql.Register(driver,
			&sqlite3.SQLiteDriver{
				ConnectHook: func(conn *sqlite3.SQLiteConn) error {
					logger.Debug.Printf("Initializing snowball stemmer")
					err := snowball.Init(conn, snowball.Settings{
						Stemmers:         cfg.Stemmer.Languages,
						RemoveDiacritics: cfg.Stemmer.RemoveDiacritics,
						TokenCharacters:  cfg.Stemmer.TokenCharacters,
						Separators:       cfg.Stemmer.Separators,
					})

					if err != nil {
						return err
					}
					logger.Debug.Printf("Initializing aux functions")
					err = auxilliary.Init(conn)
					return err
				},
			})
	}
}

type connectionMode bool

const (
	readOnly  connectionMode = true
	readWrite connectionMode = false
)

func getDatabaseURL(dbPath string, mode connectionMode) (string, error) {
	abspath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("Failed to get absolute path to DB: %w", err)
	}
	escapedPath := strings.Replace(abspath, " ", "%20", -1)

	if mode == readOnly {
		return fmt.Sprintf("file:%s?_journal=WAL&_query_only=true&_foreign_keys=true&_timeout=500&cache=shared", escapedPath), nil
	}
	return fmt.Sprintf("file:%s?_journal=WAL&_foreign_keys=true&_timeout=500&cache=private&_sync=1&_rt=true", escapedPath), nil
}

func openMigrationConnection(dbPath string) (db *sqlx.DB, err error) {
	url, err := getDatabaseURL(dbPath, readWrite)
	if err != nil {
		return nil, err
	}
	db, err = sqlx.Connect(driver, url)
	return
}

func openDatabase(dbPath string, spaces []string) (rdb *sqlx.DB, wdb *sqlx.DB, err error) {

	// Only one writer
	writeSqliteURL, err := getDatabaseURL(dbPath, readWrite)
	if err != nil {
		return nil, nil, err
	}
	wdb, err = sqlx.Connect(driver, writeSqliteURL)
	if err != nil {
		return
	}
	wdb.SetMaxOpenConns(1)

	// Multiple readers
	readSqliteURL, err := getDatabaseURL(dbPath, readOnly)
	rdb, err = sqlx.Connect(driver, readSqliteURL)
	if err != nil {
		return
	}

	if len(spaces) < 1 {
		return nil, nil, fmt.Errorf("No spaces defined: %v", spaces)
	}
	err = initDB(wdb, writeSqliteURL, spaces)
	return
}
