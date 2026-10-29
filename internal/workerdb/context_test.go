package workerdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWALReadersSeeCommittedSnapshotDuringPendingWrite(t *testing.T) {
	db := openTestDB(t)
	name, key := []byte("timings"), []byte("item")
	if err := db.Update(func(tx *Tx) error {
		b, err := tx.CreateBucketIfNotExists(name)
		if err != nil {
			return err
		}
		return b.Put(key, []byte("committed"))
	}); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- db.Update(func(tx *Tx) error {
			if err := tx.Bucket(name).Put(key, []byte("indexed")); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	released := false
	defer func() {
		if !released {
			close(release)
			<-done
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("index writer did not reach its held transaction")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := db.ViewContext(ctx, func(tx *Tx) error {
		if got := string(tx.Bucket(name).Get(key)); got != "committed" {
			return fmt.Errorf("reader saw uncommitted index write: %q", got)
		}
		// Enforce read-only access in SQLite as well as the bucket interface.
		if _, err := tx.tx.ExecContext(tx.ctx, "DELETE FROM entries"); err == nil {
			return errors.New("read-only pool accepted a direct mutation")
		}
		close(release)
		released = true
		if err := <-done; err != nil {
			return err
		}
		if got := string(tx.Bucket(name).Get(key)); got != "committed" {
			return fmt.Errorf("concurrent commit replaced the reader's snapshot: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("reader waited for the pending writer: %v", err)
	}
	if err := db.View(func(tx *Tx) error {
		if got := string(tx.Bucket(name).Get(key)); got != "indexed" {
			return fmt.Errorf("next snapshot missed the committed index: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReadContextCancelsPoolWaitAndStreamingStatement(t *testing.T) {
	db := openTestDB(t)
	name := []byte("timings")
	if err := db.Update(func(tx *Tx) error {
		b, err := tx.CreateBucketIfNotExists(name)
		if err != nil {
			return err
		}
		for i := 0; i < 32; i++ {
			if err := b.Put([]byte(fmt.Sprint(i)), []byte("value")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}, 4), make(chan struct{}), make(chan error, 4)
	released := false
	defer func() {
		if !released {
			close(release)
			for range 4 {
				<-done
			}
		}
	}()
	for range 4 {
		go func() {
			done <- db.View(func(tx *Tx) error {
				_ = tx.Bucket(name).Get([]byte("0"))
				entered <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("read-only pool did not admit four bounded readers")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	err := db.ViewContext(ctx, func(*Tx) error { t.Error("canceled reader entered a saturated pool"); return nil })
	cancel()
	close(release)
	released = true
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader pool wait ignored context: %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	err = db.ViewContext(ctx, func(tx *Tx) error {
		return tx.Bucket(name).ForEach(func(_, _ []byte) error { cancel(); return nil })
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("streaming read ignored context: %v", err)
	}
	// Cancellation must not poison either pool for later work.
	if err := db.Update(func(tx *Tx) error { return tx.Bucket(name).Put([]byte("after"), []byte("ok")) }); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *Tx) error {
		if string(tx.Bucket(name).Get([]byte("after"))) != "ok" {
			return errors.New("reader pool did not recover after cancellation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledTransactionsReturnContextErrorAfterRollback(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprintf("write=%t", write), func(t *testing.T) {
			db := openTestDB(t)
			name := []byte("cancellation")
			if err := db.Update(func(tx *Tx) error {
				_, err := tx.CreateBucketIfNotExists(name)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := db.transaction(ctx, write, func(tx *Tx) error {
				if write {
					if err := tx.Bucket(name).Put([]byte("aborted"), []byte("value")); err != nil {
						return err
					}
				}
				cancel()
				// Force the ordering in which rollback completes before the
				// wrapper calls Commit, without depending on scheduler timing.
				if err := tx.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
					return err
				}
				return nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("completed canceled transaction = %v", err)
			}
			if err := db.Update(func(tx *Tx) error {
				return tx.Bucket(name).Put([]byte("after"), []byte("committed"))
			}); err != nil {
				t.Fatalf("writer pool did not recover: %v", err)
			}
			if err := db.View(func(tx *Tx) error {
				bucket := tx.Bucket(name)
				if bucket.Get([]byte("aborted")) != nil {
					t.Error("canceled transaction committed a write")
				}
				if string(bucket.Get([]byte("after"))) != "committed" {
					t.Error("reader pool missed a later committed write")
				}
				return nil
			}); err != nil {
				t.Fatalf("reader pool did not recover: %v", err)
			}
		})
	}
}

func TestCancellationPreservesCallbackAndSQLErrors(t *testing.T) {
	t.Run("callback", func(t *testing.T) {
		db := openTestDB(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		want := errors.New("callback failure")
		err := db.UpdateContext(ctx, func(*Tx) error {
			cancel()
			return want
		})
		if err != want {
			t.Fatalf("callback error was masked by cancellation: %v", err)
		}
	})
	t.Run("SQL", func(t *testing.T) {
		db := openTestDB(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var want error
		err := db.UpdateContext(ctx, func(tx *Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte("test"))
			if err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, "DROP TABLE entries"); err != nil {
				return err
			}
			_ = bucket.Get([]byte("missing"))
			want = tx.err
			if want == nil {
				t.Fatal("missing table did not cause a retained SQL error")
			}
			cancel()
			return nil
		})
		if err != want {
			t.Fatalf("SQL error was masked by cancellation: %v", err)
		}
		if err := db.Update(func(tx *Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte("after"))
			if err != nil {
				return err
			}
			return bucket.Put([]byte("key"), []byte("value"))
		}); err != nil {
			t.Fatalf("failed transaction changed the schema: %v", err)
		}
	})
}

func TestCommitCancellationPreservesOutcome(t *testing.T) {
	commitFailure := errors.New("driver commit failure")
	for _, test := range []struct {
		name   string
		cancel bool
		commit error
		want   error
	}{
		{name: "canceled rollback", cancel: true, commit: sql.ErrTxDone, want: context.Canceled},
		{name: "completed transaction", commit: sql.ErrTxDone, want: sql.ErrTxDone},
		{name: "successful commit", cancel: true},
		{name: "failed commit", cancel: true, commit: commitFailure, want: commitFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pool := sql.OpenDB(commitTestDriver{commit: func() error {
				if test.cancel {
					cancel()
				}
				return test.commit
			}})
			t.Cleanup(func() { _ = pool.Close() })
			db := &DB{db: pool}
			if err := db.UpdateContext(ctx, func(*Tx) error { return nil }); err != test.want {
				t.Fatalf("transaction outcome = %v, want %v", err, test.want)
			}
		})
	}
}

// A private connector makes cancellation during the driver Commit deterministic.
type commitTestDriver struct{ commit func() error }

func (d commitTestDriver) Open(string) (driver.Conn, error) { return commitTestConn{d}, nil }
func (d commitTestDriver) Connect(context.Context) (driver.Conn, error) {
	return d.Open("")
}
func (d commitTestDriver) Driver() driver.Driver { return d }

type commitTestConn struct{ commitTestDriver }

func (c commitTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("test connector does not support statements")
}
func (c commitTestConn) Close() error              { return nil }
func (c commitTestConn) Begin() (driver.Tx, error) { return commitTestTx{c.commit}, nil }

type commitTestTx struct{ commit func() error }

func (tx commitTestTx) Commit() error   { return tx.commit() }
func (tx commitTestTx) Rollback() error { return nil }
