package panopticon

import "fmt"

type FlowError struct{ Message string }

func (e *FlowError) Error() string { return e.Message }

func flowError(format string, args ...any) error {
	return &FlowError{Message: fmt.Sprintf(format, args...)}
}
