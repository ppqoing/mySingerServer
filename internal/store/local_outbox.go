package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type LocalOutboxEvent struct {
	Topic       string
	EntityKey   string
	Generation  int64
	PayloadJSON string
}

func (d *DB) EnqueueLocalEvent(ctx context.Context, event LocalOutboxEvent) error {
	if event.Topic == "" || event.EntityKey == "" || event.Generation < 0 {
		return fmt.Errorf("store: invalid local outbox event identity")
	}
	if !json.Valid([]byte(event.PayloadJSON)) {
		return fmt.Errorf("store: invalid local outbox payload")
	}
	now := time.Now().UnixMilli()
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO local_outbox
			(topic,entity_key,generation,payload_json,created_at,updated_at)
		VALUES (?1,?2,?3,?4,?5,?5)
		ON CONFLICT(topic,entity_key,generation) DO NOTHING`,
		event.Topic, event.EntityKey, event.Generation, event.PayloadJSON, now,
	); err != nil {
		return fmt.Errorf("store: enqueue local event: %w", err)
	}
	return nil
}
