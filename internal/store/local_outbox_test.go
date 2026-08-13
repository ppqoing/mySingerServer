package store

import (
	"context"
	"sync"
	"testing"
)

func TestLocalOutboxConcurrentEnqueueIsIdempotent(t *testing.T) {
	db := openLocalTestDB(t)
	event := LocalOutboxEvent{
		Topic: "local.review", EntityKey: "review-1", Generation: 7,
		PayloadJSON: `{"decision":"keep"}`,
	}
	const callers = 12
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := db.EnqueueLocalEvent(context.Background(), event); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("EnqueueLocalEvent: %v", err)
	}
	var count, generation, retryCount int
	var ack any
	if err := db.db.QueryRow(`
		SELECT count(*),generation,retry_count,ack_at
		FROM local_outbox WHERE topic=? AND entity_key=?`, event.Topic, event.EntityKey,
	).Scan(&count, &generation, &retryCount, &ack); err != nil {
		t.Fatal(err)
	}
	if count != 1 || generation != 7 || retryCount != 0 || ack != nil {
		t.Fatalf("outbox = count:%d generation:%d retry:%d ack:%v", count, generation, retryCount, ack)
	}
}

func TestLocalOutboxRejectsMalformedPayload(t *testing.T) {
	db := openLocalTestDB(t)
	err := db.EnqueueLocalEvent(context.Background(), LocalOutboxEvent{
		Topic: "local.task", EntityKey: "task-1", Generation: 1,
		PayloadJSON: `{broken`,
	})
	if err == nil {
		t.Fatal("malformed outbox payload was accepted")
	}
}
