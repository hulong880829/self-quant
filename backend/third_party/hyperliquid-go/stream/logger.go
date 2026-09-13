package stream

// Logger is the leveled-logging surface the SDK uses for internal warnings
// and diagnostic output. Supply one with Client.SetLogger; the default is
// a silent no-op.
type Logger interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// nopLogger discards every log call.
type nopLogger struct{}

// Debugf discards format/args.
func (nopLogger) Debugf(string, ...any) {}

// Infof discards format/args.
func (nopLogger) Infof(string, ...any) {}

// Warnf discards format/args.
func (nopLogger) Warnf(string, ...any) {}

// Errorf discards format/args.
func (nopLogger) Errorf(string, ...any) {}
