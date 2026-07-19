package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// createTestWebhook inserts a webhook subscribed to the given events and
// returns its id. secret_enc is opaque here (the store never decrypts it).
func createTestWebhook(t *testing.T, q *store.Queries, active bool, events ...string) uuid.UUID {
	t.Helper()
	wh, err := q.CreateWebhook(context.Background(), store.CreateWebhookParams{
		Name:      "test-endpoint",
		Url:       fmt.Sprintf("https://example.test/hooks/%s", uuid.NewString()[:8]),
		SecretEnc: "v1:not-a-real-ciphertext",
		Events:    events,
		Active:    active,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return wh.ID
}

// TestWebhookDeliveryStateTransitions walks a delivery from pending through an
// intermediate retry to terminal success, asserting attempts increments and
// the success/error/delivered_at columns at each step.
func TestWebhookDeliveryStateTransitions(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	webhookID := createTestWebhook(t, q, true, "ticket.created")

	del, err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{
		WebhookID: webhookID,
		EventType: "ticket.created",
		Payload:   []byte(`{"ticket":"1"}`),
		// ticket_id left null (pgtype.UUID zero value, Valid=false).
	})
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	if del.Status != store.WebhookDeliveryStatusPending {
		t.Fatalf("fresh delivery status = %q, want pending", del.Status)
	}
	if del.Attempts != 0 {
		t.Fatalf("fresh delivery attempts = %d, want 0", del.Attempts)
	}
	if del.DeliveredAt.Valid {
		t.Fatalf("fresh delivery has delivered_at set")
	}
	if del.TicketID.Valid {
		t.Fatalf("null ticket_id came back Valid")
	}

	// Intermediate failure: stays pending, attempts -> 1, records code+error.
	if err := q.RecordWebhookDeliveryRetry(ctx, store.RecordWebhookDeliveryRetryParams{
		ID:           del.ID,
		ResponseCode: pgtype.Int4{Int32: 503, Valid: true},
		LastError:    pgtype.Text{String: "503 Service Unavailable", Valid: true},
	}); err != nil {
		t.Fatalf("record retry: %v", err)
	}
	got := getDelivery(t, q, del.ID)
	if got.Status != store.WebhookDeliveryStatusPending {
		t.Errorf("after retry status = %q, want pending", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("after retry attempts = %d, want 1", got.Attempts)
	}
	if !got.ResponseCode.Valid || got.ResponseCode.Int32 != 503 {
		t.Errorf("after retry response_code = %+v, want 503", got.ResponseCode)
	}
	if got.LastError.String != "503 Service Unavailable" {
		t.Errorf("after retry last_error = %q", got.LastError.String)
	}
	if got.DeliveredAt.Valid {
		t.Errorf("after retry delivered_at should be null")
	}

	// Terminal success: attempts -> 2, delivered_at set, last_error cleared.
	if err := q.MarkWebhookDeliverySuccess(ctx, store.MarkWebhookDeliverySuccessParams{
		ID:           del.ID,
		ResponseCode: pgtype.Int4{Int32: 200, Valid: true},
	}); err != nil {
		t.Fatalf("mark success: %v", err)
	}
	got = getDelivery(t, q, del.ID)
	if got.Status != store.WebhookDeliveryStatusSuccess {
		t.Errorf("after success status = %q, want success", got.Status)
	}
	if got.Attempts != 2 {
		t.Errorf("after success attempts = %d, want 2", got.Attempts)
	}
	if !got.ResponseCode.Valid || got.ResponseCode.Int32 != 200 {
		t.Errorf("after success response_code = %+v, want 200", got.ResponseCode)
	}
	if got.LastError.Valid {
		t.Errorf("after success last_error should be cleared, got %q", got.LastError.String)
	}
	if !got.DeliveredAt.Valid {
		t.Errorf("after success delivered_at should be set")
	}
}

// TestWebhookDeliveryTerminalFailure covers a transport-level failure with no
// HTTP response: status -> failed, attempts incremented, response_code null.
func TestWebhookDeliveryTerminalFailure(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	webhookID := createTestWebhook(t, q, true, "article.created")

	del, err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{
		WebhookID: webhookID,
		EventType: "article.created",
		Payload:   []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}

	if err := q.MarkWebhookDeliveryFailed(ctx, store.MarkWebhookDeliveryFailedParams{
		ID:           del.ID,
		ResponseCode: pgtype.Int4{}, // null: connection refused, no response
		LastError:    pgtype.Text{String: "dial tcp: connection refused", Valid: true},
	}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got := getDelivery(t, q, del.ID)
	if got.Status != store.WebhookDeliveryStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	if got.ResponseCode.Valid {
		t.Errorf("response_code should be null for a transport error, got %d", got.ResponseCode.Int32)
	}
	if got.LastError.String != "dial tcp: connection refused" {
		t.Errorf("last_error = %q", got.LastError.String)
	}
	if got.DeliveredAt.Valid {
		t.Errorf("failed delivery should not stamp delivered_at")
	}
}

// TestListActiveWebhooksByEvent verifies the delivery fan-out only returns
// active endpoints subscribed to the exact event.
func TestListActiveWebhooksByEvent(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)

	wantID := createTestWebhook(t, q, true, "ticket.created", "ticket.updated")
	createTestWebhook(t, q, false, "ticket.created") // inactive: excluded
	createTestWebhook(t, q, true, "article.created") // wrong event: excluded

	rows, err := q.ListActiveWebhooksByEvent(ctx, "ticket.created")
	if err != nil {
		t.Fatalf("list active by event: %v", err)
	}
	var found bool
	for _, w := range rows {
		if w.ID == wantID {
			found = true
		}
		if !w.Active {
			t.Errorf("inactive webhook %s returned", w.ID)
		}
		if !contains(w.Events, "ticket.created") {
			t.Errorf("webhook %s not subscribed to ticket.created: %v", w.ID, w.Events)
		}
	}
	if !found {
		t.Errorf("expected webhook %s in results", wantID)
	}
}

func TestListRecentWebhookDeliveries(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	webhookID := createTestWebhook(t, q, true, "ticket.created")

	var ids []int64
	for i := 0; i < 3; i++ {
		del, err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{
			WebhookID: webhookID,
			EventType: "ticket.created",
			Payload:   []byte(fmt.Sprintf(`{"n":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("insert #%d: %v", i, err)
		}
		ids = append(ids, del.ID)
	}

	rows, err := q.ListRecentWebhookDeliveries(ctx, store.ListRecentWebhookDeliveriesParams{
		WebhookID: webhookID,
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("returned %d rows, want 2 (limit)", len(rows))
	}
	// Newest first: last inserted id should lead.
	if rows[0].ID != ids[2] || rows[1].ID != ids[1] {
		t.Errorf("ordering = [%d %d], want newest-first [%d %d]", rows[0].ID, rows[1].ID, ids[2], ids[1])
	}
}

func getDelivery(t *testing.T, q *store.Queries, id int64) store.WebhookDelivery {
	t.Helper()
	got, err := q.GetWebhookDelivery(context.Background(), id)
	if err != nil {
		t.Fatalf("get delivery %d: %v", id, err)
	}
	return got
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
