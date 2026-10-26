package workerdb

import (
	"context"
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
