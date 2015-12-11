package libfuse

import (
	"encoding/json"
	"time"

	"bazil.org/fuse"
	"github.com/go-errors/errors"
	"github.com/keybase/kbfs/libkbfs"
)

// jsonReportedError stringifies the reported error before marshalling
type jsonReportedError struct {
	Level libkbfs.ReportingLevel
	Time  time.Time
	Error string
	Stack []errors.StackFrame
}

func convertStack(stack []uintptr) []errors.StackFrame {
	frames := make([]errors.StackFrame, len(stack))
	for i, pc := range stack {
		// TODO: Handle panics correctly, as described in the
		// docs for runtime.Callers().
		frames[i] = errors.NewStackFrame(pc)
	}
	return frames
}

func getEncodedErrors(fs *FS) (data []byte, t time.Time, err error) {
	errors := fs.config.Reporter().AllKnownErrors()
	jsonErrors := make([]jsonReportedError, len(errors))
	for i, e := range errors {
		jsonErrors[i].Level = e.Level
		jsonErrors[i].Time = e.Time
		jsonErrors[i].Error = e.Error.String()
		jsonErrors[i].Stack = convertStack(e.Stack)
	}
	data, err = json.MarshalIndent(jsonErrors, "", "  ")
	if err != nil {
		return nil, time.Time{}, err
	}
	data = append(data, '\n')
	if len(errors) > 0 {
		t = errors[len(errors)-1].Time
	}
	return data, t, err
}

// NewErrorFile returns a special read file that contains a text
// representation of the last few KBFS errors.
func NewErrorFile(fs *FS, resp *fuse.LookupResponse) *SpecialReadFile {
	resp.EntryValid = 0
	return &SpecialReadFile{
		read: func() ([]byte, time.Time, error) {
			return getEncodedErrors(fs)
		},
	}
}
