package raftdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/robinmail/robinmail/internal/db"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// openSQLite opens a database file with the settings every file here
// gets: WAL, a busy timeout, foreign keys off (as rqlite had them).
// synchronous is FULL for the Raft log and NORMAL for the data, whose
// writes the log can redo. readOnly connections refuse to write.
func openSQLite(path, synchronous string, readOnly bool, conns int) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous("+synchronous+")")
	q.Add("_pragma", "foreign_keys(0)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	}
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(conns)
	d.SetMaxIdleConns(conns)
	d.SetConnMaxLifetime(0)
	d.SetConnMaxIdleTime(0)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// querier is a connection or a transaction.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// run runs one statement and returns its result. A statement that returns
// columns gives its rows. Any other gives the count of rows it changed:
// changes() and total_changes() are read right after it on the same
// connection, and *total, the connection's total_changes() before it,
// tells whether it changed anything at all, so that a statement that did
// not (DDL, an INSERT OR IGNORE that ignored) reports 0 and not what an
// earlier statement left in changes(). LastInsertID is set only for an
// INSERT or REPLACE that inserted: last_insert_rowid() is otherwise left
// over from whatever ran on the connection before, and would differ
// between nodes. With total nil (reads) a statement without columns
// reports nothing.
func run(ctx context.Context, q querier, st db.Statement, total *int64) (db.Result, error) {
	args, err := bind(st)
	if err != nil {
		return db.Result{}, err
	}
	rows, err := q.QueryContext(ctx, st.SQL, args...)
	if err != nil {
		return db.Result{}, err
	}
	res, err := scan(rows)
	if err != nil {
		return db.Result{}, err
	}
	if len(res.Columns) > 0 || total == nil {
		return res, nil
	}
	rows, err = q.QueryContext(ctx, `SELECT changes(), last_insert_rowid(), total_changes()`)
	if err != nil {
		return db.Result{}, err
	}
	n, err := scan(rows)
	if err != nil {
		return db.Result{}, err
	}
	if len(n.Values) != 1 {
		return res, nil
	}
	after, _ := n.Values[0][2].(int64)
	if after == *total {
		return res, nil
	}
	*total = after
	res.RowsAffected, _ = n.Values[0][0].(int64)
	if insert(st.SQL) {
		res.LastInsertID, _ = n.Values[0][1].(int64)
	}
	return res, nil
}

// totalChanges reads the connection's total_changes(), for run.
func totalChanges(ctx context.Context, q querier) (int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT total_changes()`)
	if err != nil {
		return 0, err
	}
	res, err := scan(rows)
	if err != nil || len(res.Values) != 1 {
		return 0, err
	}
	n, _ := res.Values[0][0].(int64)
	return n, nil
}

func insert(sql string) bool {
	s := strings.TrimLeft(sql, " \t\r\n")
	return len(s) >= 6 && (strings.EqualFold(s[:6], "INSERT") || (len(s) >= 7 && strings.EqualFold(s[:7], "REPLACE")))
}

// bind turns a statement's parameters into arguments for database/sql:
// positional ones as they are (SQLite numbers "?" and "?N" itself),
// named ones as sql.Named.
func bind(st db.Statement) ([]any, error) {
	if len(st.Named) > 0 {
		if len(st.Args) > 0 {
			return nil, errors.New("statement has both positional and named parameters")
		}
		out := make([]any, 0, len(st.Named))
		for k, v := range st.Named {
			out = append(out, sql.Named(k, value(v)))
		}
		return out, nil
	}
	out := make([]any, len(st.Args))
	for i, v := range st.Args {
		out[i] = value(v)
	}
	return out, nil
}

// value is a parameter as SQLite gets it. After db.Normalize a value is
// nil, int64, float64, string or bool; a bool is 1 or 0.
func value(v any) any {
	if b, ok := v.(bool); ok {
		if b {
			return int64(1)
		}
		return int64(0)
	}
	return v
}

// scan reads all rows. A value is nil, int64, float64 or string: BLOB
// and anything else the driver makes is turned into text.
func scan(rows *sql.Rows) (db.Result, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return db.Result{}, err
	}
	res := db.Result{Columns: cols}
	if len(cols) == 0 {
		// Step it to the end: a write without columns runs here.
		for rows.Next() {
		}
		return db.Result{}, rows.Err()
	}
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return db.Result{}, err
		}
		for i, v := range row {
			switch x := v.(type) {
			case nil, int64, float64, string:
			case []byte:
				row[i] = string(x)
			case bool:
				if x {
					row[i] = int64(1)
				} else {
					row[i] = int64(0)
				}
			default:
				row[i] = fmt.Sprint(x)
			}
		}
		res.Values = append(res.Values, row)
	}
	return res, rows.Err()
}

// sqliteCode returns SQLite's extended result code in err, or 0.
func sqliteCode(err error) int {
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return 0
}

// statementError makes the error of statement i into a *db.Error when it
// is an answer about the statement, and reports false when it is a fault
// of this node (I/O, a full disk, a damaged file, no memory): such an
// error must not become part of the replicated outcome, since another
// node would not have had it.
func statementError(i int, err error) (*db.Error, bool) {
	code := sqliteCode(err)
	switch code & 0xff {
	case sqlite3.SQLITE_IOERR, sqlite3.SQLITE_FULL, sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOMEM,
		sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_NOTADB,
		sqlite3.SQLITE_PROTOCOL, sqlite3.SQLITE_READONLY, sqlite3.SQLITE_INTERRUPT, sqlite3.SQLITE_ABORT:
		return nil, false
	}
	if code == 0 && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		// A context ended: not an answer about the statement.
		return nil, false
	}
	return &db.Error{Index: i, Message: message(err), Code: code}, true
}

// message is SQLite's own message, without the driver's wrapping:
// "constraint failed: UNIQUE constraint failed: t.c (2067)" becomes
// "UNIQUE constraint failed: t.c".
func message(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		s = s[:i]
	}
	if _, rest, ok := strings.Cut(s, ": "); ok && sqliteCode(err) != 0 {
		s = rest
	}
	return s
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
