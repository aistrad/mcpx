package config

import (
	"testing"
	"time"
)

func TestEffectiveAuthMode(t *testing.T) {
	if EffectiveAuthMode(AuthConfig{}) != "open" {
		t.Fatal("open")
	}
	if EffectiveAuthMode(AuthConfig{Token: "x"}) != "bearer" {
		t.Fatal("bearer")
	}
	if EffectiveAuthMode(AuthConfig{Mode: "dual", Token: "x"}) != "dual" {
		t.Fatal("dual")
	}
	if ValidateAuthMode(AuthConfig{Mode: "nope"}) == nil {
		t.Fatal("expect invalid")
	}
}

func TestTransportSessionIdleTTLDefaultsToDay(t *testing.T) {
	if got := TransportSessionIdleTTL(TransportConfig{}); got != 24*time.Hour {
		t.Fatalf("default TTL = %s", got)
	}
	if got := TransportSessionIdleTTL(TransportConfig{SessionIdleTTL: "15m"}); got != 15*time.Minute {
		t.Fatalf("configured TTL = %s", got)
	}
	if got := TransportSessionIdleTTL(TransportConfig{SessionIdleTTL: "invalid"}); got != 24*time.Hour {
		t.Fatalf("invalid TTL = %s", got)
	}
}

func TestWriterLeaseTTLIsBounded(t *testing.T) {
	if got := WriterLeaseTTL(TransportConfig{}); got != 2*time.Minute {
		t.Fatalf("default writer lease TTL = %s", got)
	}
	if got := WriterLeaseTTL(TransportConfig{WriterLeaseTTL: "10m"}); got != 10*time.Minute {
		t.Fatalf("configured writer lease TTL = %s", got)
	}
	if got := WriterLeaseTTL(TransportConfig{WriterLeaseTTL: "1s"}); got != 2*time.Minute {
		t.Fatalf("too-short writer lease TTL = %s", got)
	}
}
