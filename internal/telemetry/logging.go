package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.uber.org/zap/zapcore"
)

// logFlushTimeout bounds a synchronous flush of buffered log records to the
// collector, either on Sync or before the process exits on a Fatal log.
const logFlushTimeout = 5 * time.Second

type LoggerOption func(d *customLogger)

func WithLogOTLPEndpoint(endpoint string) LoggerOption {
	return func(d *customLogger) {
		d.endpoint = endpoint
	}
}

func WithLogOTLPInsecure() LoggerOption {
	return func(d *customLogger) {
		d.insecure = true
	}
}

func WithLogAttributes(attrs ...attribute.KeyValue) LoggerOption {
	return func(d *customLogger) {
		d.attributes = attrs
	}
}

type customLogger struct {
	endpoint   string
	insecure   bool
	attributes []attribute.KeyValue
}

func MustNewLoggerProvider(opts ...LoggerOption) *sdklog.LoggerProvider {
	l := &customLogger{
		attributes: []attribute.KeyValue{},
	}

	for _, opt := range opts {
		opt(l)
	}

	baseRes, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(l.attributes...))
	if err != nil {
		panic(err)
	}

	res, err := resource.Merge(baseRes, resource.Environment())
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	endpoint, schemeSecure := ParseOTLPEndpoint(l.endpoint)
	secure := ResolveOTLPSecurity(!l.insecure, schemeSecure)

	options := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(endpoint),
	}

	if !secure {
		options = append(options, otlploggrpc.WithInsecure())
	}

	// Unlike the trace exporter, which dials with grpc.WithBlock, the log
	// exporter connects lazily: an unreachable collector does not delay or
	// fail startup, and export errors surface from the batch processor
	// instead. New only fails on invalid configuration.
	exp, err := otlploggrpc.New(ctx, options...)
	if err != nil {
		panic(fmt.Sprintf("failed to establish a connection with the otlp log exporter: %v", err))
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)

	return lp
}

// NewFlushingCore wraps an otelzap bridge core so that buffered records
// actually reach the collector when it matters: the bridge's own Sync is a
// no-op, and a Fatal log os.Exits before any deferred provider shutdown can
// run, so without this the record most worth exporting would die in the
// batch processor. Sync force-flushes the provider, and — mirroring
// zapcore's ioCore — so does any write above Error level, since the program
// may be about to crash.
func NewFlushingCore(core zapcore.Core, lp *sdklog.LoggerProvider) zapcore.Core {
	return &flushingCore{Core: core, lp: lp}
}

type flushingCore struct {
	zapcore.Core
	lp *sdklog.LoggerProvider
}

func (c *flushingCore) With(fields []zapcore.Field) zapcore.Core {
	return &flushingCore{Core: c.Core.With(fields), lp: c.lp}
}

func (c *flushingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Core.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *flushingCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	err := c.Core.Write(ent, fields)
	if ent.Level > zapcore.ErrorLevel {
		return errors.Join(err, c.flush())
	}
	return err
}

func (c *flushingCore) Sync() error {
	return errors.Join(c.Core.Sync(), c.flush())
}

func (c *flushingCore) flush() error {
	ctx, cancel := context.WithTimeout(context.Background(), logFlushTimeout)
	defer cancel()
	return c.lp.ForceFlush(ctx)
}
