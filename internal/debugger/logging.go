package debugger

import (
	"bytes"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// logFactory implements act's runner.JobLoggerFactory. act calls WithJobLogger to
// obtain the logger for the job (step loggers derive from it), so returning a
// logger that writes to our lineWriter routes all of act's output into the
// session's log channel instead of stderr — essential for a TUI that owns the
// terminal. act wraps our formatter with its secret-masking formatter, so masking
// still applies.
type logFactory struct {
	w *lineWriter
}

func (f *logFactory) WithJobLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(f.w)
	l.SetLevel(logrus.InfoLevel)
	l.SetFormatter(messageFormatter{})
	return l
}

// messageFormatter renders just the (already secret-masked) message, one line per
// entry — clean enough for a log pane. act's color/prefix formatter is unexported,
// so we keep our own minimal one.
type messageFormatter struct{}

func (messageFormatter) Format(e *logrus.Entry) ([]byte, error) {
	return append([]byte(e.Message), '\n'), nil
}

// lineWriter buffers writes and emits complete lines to sink. It is safe for
// concurrent use and never blocks past the run: sends abandon on stop.
type lineWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	sink chan<- string
	stop <-chan struct{}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// Partial line with no newline yet — keep it for the next write.
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		select {
		case w.sink <- strings.TrimRight(line, "\r\n"):
		case <-w.stop:
			return len(p), nil
		}
	}
	return len(p), nil
}
