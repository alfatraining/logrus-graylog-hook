package graylog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
)

// Set graylog.BufSize = <value> _before_ calling NewGraylogHook
// Once the buffer is full, logging will start blocking, waiting for slots to
// be available in the queue.
var BufSize uint = 8192

// 0	Emergency: system is unusable
// 1	Alert: action must be taken immediately
// 2	Critical: critical conditions
// 3	Error: error conditions
// 4	Warning: warning conditions
// 5	Notice: normal but significant condition
// 6	Informational: informational messages
// 7	Debug: debug-level messages
var levelMap = map[logrus.Level]int32{logrus.PanicLevel: 0, logrus.FatalLevel: 2, logrus.ErrorLevel: 3, logrus.InfoLevel: 6, logrus.WarnLevel: 4, logrus.DebugLevel: 7}

// GraylogHook to send logs to a logging service compatible with the Graylog API and the GELF format.
type GraylogHook struct {
	Extra       map[string]interface{}
	gelfLogger  *Writer
	buf         chan graylogEntry
	wg          sync.WaitGroup
	mu          sync.RWMutex
	synchronous bool
	blacklist   map[string]bool
}

// Graylog needs file and line params
type graylogEntry struct {
	*logrus.Entry
	file string
	line int
}

// NewGraylogHook creates a hook to be added to an instance of logger.
func NewGraylogHook(addr string, extra map[string]interface{}) *GraylogHook {
	g, err := NewWriter(addr)
	if err != nil {
		logrus.WithField("err", err).Info("Can't create Gelf logger")
		return nil
	}
	hook := &GraylogHook{
		Extra:       extra,
		gelfLogger:  g,
		synchronous: true,
	}
	return hook
}

// NewAsyncGraylogHook creates a hook to be added to an instance of logger.
// The hook created will be asynchronous, and it's the responsibility of the user to call the Flush method
// before exiting to empty the log queue.
func NewAsyncGraylogHook(addr string, extra map[string]interface{}) *GraylogHook {
	g, err := NewWriter(addr)
	if err != nil {
		logrus.WithField("err", err).Info("Can't create Gelf logger")
		return nil
	}
	hook := &GraylogHook{
		Extra:      extra,
		gelfLogger: g,
		buf:        make(chan graylogEntry, BufSize),
	}
	go hook.fire() // Log in background
	return hook
}

// Fire is called when a log event is fired.
// We assume the entry will be altered by another hook,
// otherwise we might logging something wrong to Graylog
func (hook *GraylogHook) Fire(entry *logrus.Entry) error {
	hook.mu.RLock() // Claim the mutex as a RLock - allowing multiple go routines to log simultaneously
	defer hook.mu.RUnlock()

	// get caller file and line here, it won't be available inside the goroutine
	// 1 for the function that called us.
	file, line := getCallerIgnoringLogMulti(1)

	newData := make(map[string]interface{})
	for k, v := range entry.Data {
		newData[k] = v
	}

	newEntry := &logrus.Entry{
		Logger:  entry.Logger,
		Data:    newData,
		Time:    entry.Time,
		Level:   entry.Level,
		Message: entry.Message,
	}
	gEntry := graylogEntry{newEntry, file, line}

	if hook.synchronous {
		hook.sendEntry(gEntry)
	} else {
		hook.wg.Add(1)
		hook.buf <- gEntry
	}

	return nil
}

// Flush waits for the log queue to be empty.
// This func is meant to be used when the hook was created with NewAsyncGraylogHook.
func (hook *GraylogHook) Flush() {
	hook.mu.Lock() // claim the mutex as a Lock - we want exclusive access to it
	defer hook.mu.Unlock()

	hook.wg.Wait()
}

// [ks] - format based on type
func formatForJSON(value interface{}) interface{} {
	switch value.(type) {
	case int:
		return value
	case float64:
		return value
	case bool:
		return value
	case string:
		return value
	default:
		return fmt.Sprintf("%s", value)
	}
}

// fire will loop on the 'buf' channel, and write entries to graylog
func (hook *GraylogHook) fire() {
	for {
		entry := <-hook.buf // receive new entry on channel
		hook.sendEntry(entry)
		hook.wg.Done()
	}
}

// sendEntry sends an entry to graylog synchronously
func (hook *GraylogHook) sendEntry(entry graylogEntry) {
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}

	w := hook.gelfLogger

	// remove trailing and leading whitespace
	p := bytes.TrimSpace([]byte(entry.Message))

	// If there are newlines in the message, use the first line
	// for the short message and set the full message to the
	// original input.  If the input has no newlines, stick the
	// whole thing in Short.
	short := p
	full := []byte("")
	if i := bytes.IndexRune(p, '\n'); i > 0 {
		short = p[:i]
		full = p
	}

	// map logrus to syslog levels
	level, ok := levelMap[entry.Level]
	if ok == false {
		level = levelMap[logrus.InfoLevel]
	}

	// Don't modify entry.Data directly, as the entry will used after this hook was fired
	extra := map[string]interface{}{}

	// Merge extra fields
	for k, v := range hook.Extra {
		k = fmt.Sprintf("_%s", k) // "[...] every field you send and prefix with a _ (underscore) will be treated as an additional field."
		extra[k] = formatForJSON(v)
	}
	for k, v := range entry.Data {
		if !hook.blacklist[k] {
			extraK := fmt.Sprintf("_%s", k) // "[...] every field you send and prefix with a _ (underscore) will be treated as an additional field."
			if k == logrus.ErrorKey {
				asError, isError := v.(error)
				_, isMarshaler := v.(json.Marshaler)
				if isError && !isMarshaler {
					extra[extraK] = newMarshalableError(asError)
				} else {
					extra[extraK] = formatForJSON(v)
				}
			} else {
				extra[extraK] = formatForJSON(v)
			}
		}
	}

	// add the logrus Level as a field in order to have the name of the level as well... I can't watch levels as numbers anymore
	extra["_severity"] = fmt.Sprintf("%s", entry.Level)

	m := Message{
		Version:  "1.1",
		Host:     host,
		Short:    string(short),
		Full:     string(full),
		TimeUnix: float64(time.Now().UnixNano()/1000000) / 1000.,
		Level:    level,
		File:     entry.file,
		Line:     entry.line,
		Extra:    extra,
	}

	if err := w.WriteMessage(&m); err != nil {
		fmt.Println(err)
	}

}

// Levels returns the available logging levels.
func (hook *GraylogHook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
		logrus.InfoLevel,
		logrus.DebugLevel,
	}
}

// Blacklist create a blacklist map to filter some message keys.
// This useful when you want your application to log extra fields locally
// but don't want graylog to store them.
func (hook *GraylogHook) Blacklist(b []string) {
	hook.blacklist = make(map[string]bool)
	for _, elem := range b {
		hook.blacklist[elem] = true
	}
}

// SetWriter sets the hook Gelf Writer
func (hook *GraylogHook) SetWriter(w *Writer) error {
	if w == nil {
		return errors.New("writer can't be nil")
	}
	hook.gelfLogger = w
	return nil
}

// Writer returns the logger Gelf Writer
func (hook *GraylogHook) Writer() *Writer {
	return hook.gelfLogger
}

// getCaller returns the filename and the line info of a function
// further down in the call stack.  Passing 0 in as callDepth would
// return info on the function calling getCallerIgnoringLog, 1 the
// parent function, and so on.  Any suffixes passed to getCaller are
// path fragments like "/pkg/log/log.go", and functions in the call
// stack from that file are ignored.
func getCaller(callDepth int, suffixesToIgnore ...string) (file string, line int) {
	// bump by 1 to ignore the getCaller (this) stackframe
	callDepth++
outer:
	for {
		var ok bool
		_, file, line, ok = runtime.Caller(callDepth)
		if !ok {
			file = "???"
			line = 0
			break
		}

		for _, s := range suffixesToIgnore {
			if strings.HasSuffix(file, s) {
				callDepth++
				continue outer
			}
		}
		break
	}
	return
}

func getCallerIgnoringLogMulti(callDepth int) (string, int) {
	// the +1 is to ignore this (getCallerIgnoringLogMulti) frame
	return getCaller(callDepth+1, "logrus/hooks.go", "logrus/entry.go", "logrus/logger.go", "logrus/exported.go", "asm_amd64.s")
}
