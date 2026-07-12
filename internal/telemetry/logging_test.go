package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.uber.org/zap/zapcore"
)

func TestMustNewLoggerProvider(t *testing.T) {
	// Verify the provider can be created with a valid (but unreachable)
	// endpoint and insecure mode. The gRPC connection is lazy, so this
	// should not block or fail.
	lp := MustNewLoggerProvider(
		WithLogOTLPEndpoint("localhost:4317"),
		WithLogOTLPInsecure(),
		WithLogAttributes(attribute.String("service.name", "test")),
	)
	if lp == nil {
		t.Fatal("expected non-nil LoggerProvider")
	}
	// Shutdown immediately to clean up resources.
	if err := lp.Shutdown(t.Context()); err != nil {
		t.Fatalf("unexpected error during shutdown: %v", err)
	}
}

// flushRecordingProcessor counts ForceFlush calls on the provider.
type flushRecordingProcessor struct {
	forceFlushes int
}

func (p *flushRecordingProcessor) OnEmit(context.Context, *sdklog.Record) error { return nil }
func (p *flushRecordingProcessor) Shutdown(context.Context) error               { return nil }
func (p *flushRecordingProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool {
	return true
}
func (p *flushRecordingProcessor) ForceFlush(context.Context) error {
	p.forceFlushes++
	return nil
}

func TestFlushingCore(t *testing.T) {
	proc := &flushRecordingProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })

	core := NewFlushingCore(zapcore.NewNopCore(), lp)

	// Writes at or below Error level must not flush: flushing there would
	// stall the hot path on collector round-trips.
	if err := core.Write(zapcore.Entry{Level: zapcore.ErrorLevel}, nil); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if proc.forceFlushes != 0 {
		t.Fatalf("expected no flush after Error write, got %d", proc.forceFlushes)
	}

	// A write above Error level precedes a crash or os.Exit that skips the
	// deferred provider shutdown, so it must flush synchronously.
	if err := core.Write(zapcore.Entry{Level: zapcore.FatalLevel}, nil); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if proc.forceFlushes != 1 {
		t.Fatalf("expected 1 flush after Fatal write, got %d", proc.forceFlushes)
	}

	// The otelzap bridge's Sync is a no-op; the wrapper must map Sync to a
	// provider flush.
	if err := core.Sync(); err != nil {
		t.Fatalf("unexpected sync error: %v", err)
	}
	if proc.forceFlushes != 2 {
		t.Fatalf("expected 2 flushes after Sync, got %d", proc.forceFlushes)
	}
}
