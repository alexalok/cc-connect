package codex

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type receiptRPCRequest struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
}

type receiptRPCWriter struct {
	requests chan receiptRPCRequest
	err      error
}

func (w *receiptRPCWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	var request receiptRPCRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	w.requests <- request
	return len(data), nil
}
func (*receiptRPCWriter) Close() error { return nil }

func TestAppServerSession_ReadReceiptRequiresTurnAcknowledgment(t *testing.T) {
	for _, tc := range []struct {
		name        string
		result      string
		rpcErr      *rpcError
		writeErr    error
		cancel      bool
		wantReceipt bool
	}{
		{name: "accepted", result: `{"turn":{"id":"turn-1"}}`, wantReceipt: true},
		{name: "rejected", rpcErr: &rpcError{Code: -1, Message: "turn rejected"}},
		{name: "missing turn id", result: `{"turn":{}}`},
		{name: "malformed response", result: `{"turn":false}`},
		{name: "broken pipe", writeErr: errors.New("broken pipe")},
		{name: "canceled before acknowledgment", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := &receiptRPCWriter{requests: make(chan receiptRPCRequest, 1), err: tc.writeErr}
			s := &appServerSession{ctx: ctx, stdin: writer}
			s.threadID.Store("thread-1")
			s.alive.Store(true)
			var receipts atomic.Int32
			done := make(chan error, 1)
			go func() {
				done <- s.SendWithReceipt("hello", "42", nil, nil, func() { receipts.Add(1) })
			}()
			if tc.writeErr == nil {
				var request receiptRPCRequest
				select {
				case request = <-writer.requests:
				case <-time.After(time.Second):
					t.Fatal("turn/start was not written")
				}
				if request.Method != "turn/start" {
					t.Fatalf("method = %s", request.Method)
				}
				if receipts.Load() != 0 {
					t.Fatal("pipe write falsely marked message received")
				}
				if tc.cancel {
					cancel()
				} else {
					s.handleResponse(rpcResponseEnvelope{ID: request.ID, Result: json.RawMessage(tc.result), Error: tc.rpcErr})
				}
			}
			select {
			case err := <-done:
				if (err == nil) != tc.wantReceipt {
					t.Fatalf("SendWithReceipt error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("send did not finish")
			}
			want := int32(0)
			if tc.wantReceipt {
				want = 1
			}
			if receipts.Load() != want {
				t.Fatalf("receipts = %d, want %d", receipts.Load(), want)
			}
		})
	}
}
