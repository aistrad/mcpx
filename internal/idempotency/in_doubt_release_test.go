package idempotency

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReviewInDoubtCancelledContextReleasesFlightWithoutOwnership(t *testing.T) {
	s, closeStore := testStore(t)
	defer closeStore()
	ctx := context.Background()
	key := testKey()
	if _, err := s.Claim(ctx, key, "same", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePending(ctx, key, "same", []byte(`{"prepared":true}`), nil); err != nil {
		t.Fatal(err)
	}
	waiting, err := s.Claim(ctx, key, "same", time.Hour)
	if err != nil || waiting.Kind != ClaimWait {
		t.Fatal("未创建等待者")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.MarkInDoubt(cancelled, key, "same", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("未注入取消: %v", err)
	}
	bounded, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if _, err := s.Wait(bounded, waiting, key); !errors.Is(err, ErrInDoubt) {
		t.Fatalf("等待者未释放为未知状态: %v", err)
	}
	claim, err := s.Claim(ctx, key, "same", time.Hour)
	if err != nil || claim.Kind != ClaimInDoubt || string(claim.Record.Response) != `{"prepared":true}` {
		t.Fatalf("取得重复执行权或丢失准备证据: %+v %v", claim, err)
	}
	if err := s.Complete(ctx, key, "same", StateSucceeded, []byte(`{"done":true}`), nil); err != nil {
		t.Fatal(err)
	}
	claim, err = s.Claim(ctx, key, "same", time.Hour)
	if err != nil || claim.Kind != ClaimReplay {
		t.Fatalf("回读完成后不可回放: %+v %v", claim, err)
	}
}
