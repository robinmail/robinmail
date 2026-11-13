// Phase 0 spike: can a delivery be one atomic rqlite request?
//
// It takes the account counters, assigns the thread, takes the mailbox UID and
// writes the change log in a single /db/request?transaction call, then checks
// the result after many concurrent deliveries. Throwaway code; phase 1
// replaces it with internal/rqlite and internal/store.
//
//	docker run -d -p 127.0.0.1:4001:4001 rqlite/rqlite
//	go run ./spike/rqlite
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const base = "http://127.0.0.1:4001"

type result struct {
	Error  string  `json:"error"`
	Values [][]any `json:"values"`
}

// request runs statements as one transaction. It returns the first statement
// error, if any.
func request(stmts [][]any) ([]result, error) {
	body, _ := json.Marshal(stmts)
	resp, err := http.Post(base+"/db/request?transaction&level=strong", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Error   string   `json:"error"`
		Results []result `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%s", out.Error)
	}
	for _, r := range out.Results {
		if r.Error != "" {
			return out.Results, fmt.Errorf("%s", r.Error)
		}
	}
	return out.Results, nil
}

var schema = [][]any{
	{`DROP TABLE IF EXISTS account`}, {`DROP TABLE IF EXISTS mailbox`}, {`DROP TABLE IF EXISTS email`},
	{`DROP TABLE IF EXISTS email_mailbox`}, {`DROP TABLE IF EXISTS thread_ref`}, {`DROP TABLE IF EXISTS changes`},
	{`DROP TABLE IF EXISTS delivered`},
	{`CREATE TABLE account (id TEXT PRIMARY KEY, modseq INTEGER NOT NULL, next_email INTEGER NOT NULL, next_thread INTEGER NOT NULL)`},
	{`CREATE TABLE mailbox (account_id TEXT, id INTEGER, uidnext INTEGER NOT NULL, modseq INTEGER NOT NULL, PRIMARY KEY (account_id, id))`},
	{`CREATE TABLE email (account_id TEXT, id INTEGER, thread_id INTEGER NOT NULL, message_id TEXT, modseq INTEGER NOT NULL, PRIMARY KEY (account_id, id))`},
	{`CREATE TABLE email_mailbox (account_id TEXT, email_id INTEGER, mailbox_id INTEGER, uid INTEGER NOT NULL, PRIMARY KEY (account_id, email_id, mailbox_id), UNIQUE (account_id, mailbox_id, uid))`},
	{`CREATE TABLE thread_ref (account_id TEXT, message_id TEXT, thread_id INTEGER NOT NULL, PRIMARY KEY (account_id, message_id))`},
	{`CREATE TABLE changes (account_id TEXT, modseq INTEGER, type TEXT, id INTEGER, op TEXT)`},
	{`CREATE TABLE delivered (account_id TEXT, key TEXT, PRIMARY KEY (account_id, key))`},
}

// deliver stores one message in mailbox 1 of the account. parent is the
// Message-ID it replies to, or "".
func deliver(acct, key, msgID, parent string) (emailID, threadID int64, err error) {
	const cur = `(SELECT next_email - 1 FROM account WHERE id = ?)`
	stmts := [][]any{
		// A second delivery of the same key fails here and rolls everything back.
		{`INSERT INTO delivered (account_id, key) VALUES (?, ?)`, acct, key},
		{`UPDATE account SET modseq = modseq + 1, next_email = next_email + 1, next_thread = next_thread + 1 WHERE id = ?`, acct},
		{`INSERT INTO email (account_id, id, thread_id, message_id, modseq)
		  SELECT a.id, a.next_email - 1,
		         COALESCE((SELECT thread_id FROM thread_ref WHERE account_id = a.id AND message_id IN (?, ?) ORDER BY thread_id LIMIT 1), a.next_thread - 1),
		         ?, a.modseq
		  FROM account a WHERE a.id = ?`, msgID, parent, msgID, acct},
		{`INSERT OR IGNORE INTO thread_ref (account_id, message_id, thread_id)
		  SELECT account_id, ?, thread_id FROM email WHERE account_id = ? AND id = ` + cur, msgID, acct, acct},
		{`INSERT INTO email_mailbox (account_id, email_id, mailbox_id, uid)
		  SELECT m.account_id, ` + cur + `, m.id, m.uidnext FROM mailbox m WHERE m.account_id = ? AND m.id = 1`, acct, acct},
		{`UPDATE mailbox SET uidnext = uidnext + 1, modseq = (SELECT modseq FROM account WHERE id = ?) WHERE account_id = ? AND id = 1`, acct, acct},
		{`INSERT INTO changes (account_id, modseq, type, id, op)
		  SELECT id, modseq, 'Email', next_email - 1, 'created' FROM account WHERE id = ?`, acct},
		{`INSERT INTO changes (account_id, modseq, type, id, op)
		  SELECT id, modseq, 'Mailbox', 1, 'updated' FROM account WHERE id = ?`, acct},
		{`SELECT id, thread_id FROM email WHERE account_id = ? AND id = ` + cur, acct, acct},
	}
	if parent != "" {
		stmts = append(stmts[:4], append([][]any{{`INSERT OR IGNORE INTO thread_ref (account_id, message_id, thread_id)
		  SELECT account_id, ?, thread_id FROM email WHERE account_id = ? AND id = ` + cur, parent, acct, acct}}, stmts[4:]...)...)
	}
	res, err := request(stmts)
	if err != nil {
		return 0, 0, err
	}
	row := res[len(res)-1].Values[0]
	return int64(row[0].(float64)), int64(row[1].(float64)), nil
}

func scalar(q string, args ...any) int64 {
	res, err := request([][]any{append([]any{q}, args...)})
	if err != nil {
		fail("query %s: %v", q, err)
	}
	if res[0].Values[0][0] == nil {
		return 0
	}
	return int64(res[0].Values[0][0].(float64))
}

var failed bool

func fail(format string, args ...any) {
	failed = true
	fmt.Printf("FAIL "+format+"\n", args...)
}

func check(name string, got, want int64) {
	if got != want {
		fail("%s: got %d, want %d", name, got, want)
		return
	}
	fmt.Printf("ok   %s = %d\n", name, got)
}

func main() {
	if _, err := request(schema); err != nil {
		fmt.Println("schema:", err)
		os.Exit(1)
	}
	accounts := []string{"acct-a", "acct-b"}
	for _, a := range accounts {
		if _, err := request([][]any{
			{`INSERT INTO account VALUES (?, 0, 1, 1)`, a},
			{`INSERT INTO mailbox VALUES (?, 1, 1, 0)`, a},
		}); err != nil {
			fmt.Println("seed:", err)
			os.Exit(1)
		}
	}

	const roots, repliesPerRoot, workers = 20, 15, 32
	rootThread := map[string]int64{} // acct/root message id -> thread
	for _, a := range accounts {
		for r := 0; r < roots; r++ {
			id := fmt.Sprintf("<root-%d@x>", r)
			_, th, err := deliver(a, "k-"+id, id, "")
			if err != nil {
				fail("root delivery: %v", err)
			}
			rootThread[a+id] = th
		}
	}

	type job struct{ acct, key, msgID, parent string }
	jobs := make(chan job)
	var okCount, dupCount, wrongThread, otherErr atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				_, th, err := deliver(j.acct, j.key, j.msgID, j.parent)
				switch {
				case err == nil:
					okCount.Add(1)
					if th != rootThread[j.acct+j.parent] {
						wrongThread.Add(1)
					}
				case bytes.Contains([]byte(err.Error()), []byte("UNIQUE constraint failed: delivered")):
					dupCount.Add(1)
				default:
					otherErr.Add(1)
					fmt.Println("error:", err)
				}
			}
		}()
	}
	// Every reply is sent twice with the same key, as a retrying sender would.
	for round := 0; round < 2; round++ {
		for _, a := range accounts {
			for r := 0; r < roots; r++ {
				for n := 0; n < repliesPerRoot; n++ {
					id := fmt.Sprintf("<reply-%d-%d@x>", r, n)
					jobs <- job{a, "k-" + id, id, fmt.Sprintf("<root-%d@x>", r)}
				}
			}
		}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	replies := int64(len(accounts) * roots * repliesPerRoot)
	perAcct := int64(roots + roots*repliesPerRoot)
	check("replies delivered", okCount.Load(), replies)
	check("duplicates refused", dupCount.Load(), replies)
	check("other errors", otherErr.Load(), 0)
	check("replies in the wrong thread", wrongThread.Load(), 0)
	for _, a := range accounts {
		check(a+" emails", scalar(`SELECT count(*) FROM email WHERE account_id = ?`, a), perAcct)
		check(a+" max email id (no gaps)", scalar(`SELECT max(id) FROM email WHERE account_id = ?`, a), perAcct)
		check(a+" modseq", scalar(`SELECT modseq FROM account WHERE id = ?`, a), perAcct)
		check(a+" distinct email modseqs", scalar(`SELECT count(DISTINCT modseq) FROM email WHERE account_id = ?`, a), perAcct)
		check(a+" uids", scalar(`SELECT count(DISTINCT uid) FROM email_mailbox WHERE account_id = ?`, a), perAcct)
		check(a+" max uid (no gaps)", scalar(`SELECT max(uid) FROM email_mailbox WHERE account_id = ?`, a), perAcct)
		check(a+" uidnext", scalar(`SELECT uidnext FROM mailbox WHERE account_id = ?`, a), perAcct+1)
		check(a+" threads", scalar(`SELECT count(DISTINCT thread_id) FROM email WHERE account_id = ?`, a), roots)
		check(a+" change rows", scalar(`SELECT count(*) FROM changes WHERE account_id = ?`, a), 2*perAcct)
		check(a+" delivered keys", scalar(`SELECT count(*) FROM delivered WHERE account_id = ?`, a), perAcct)
	}
	fmt.Printf("%d requests in %s with %d workers: %.0f/s\n", 2*replies, elapsed.Round(time.Millisecond), workers, float64(2*replies)/elapsed.Seconds())
	if failed {
		os.Exit(1)
	}
}
