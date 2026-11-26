package host

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/policy"
)

// Error kinds the PowerShell worker reports (worker.ps1,
// ConvertTo-HvmcError). The worker classifies only what it can tell from
// the failed cmdlet and the host; whether a shrink was refused or partial
// is decided here from the VM re-read after the write (classifyStartup),
// because that does not depend on the wording of localized messages.
//
//	kind           worker evidence                         gRPC code
//	vmms_down      vmms service not Running after a        UNAVAILABLE
//	               failed cmdlet (Get-Service probe)
//	out_of_memory  HResult 0x8007000E / 0x80070008 /       RESOURCE_EXHAUSTED on a grow,
//	               0x800705AA or OutOfMemoryException       INTERNAL otherwise
//	               anywhere in the exception chain
//	failed         any other cmdlet error                  INTERNAL, or ABORTED when the
//	                                                       re-read shows a refused shrink
//	protocol       request the worker could not decode     INTERNAL
//	startup        Import-Module Hyper-V failed            (worker restarts)
const (
	kindVMMSDown    = "vmms_down"
	kindOutOfMemory = "out_of_memory"
	kindFailed      = "failed"
	kindProtocol    = "protocol"
	kindStartup     = "startup"
)

// WorkerError is an error the PowerShell worker reported for one call. Its
// message is the cmdlet's (possibly localized) message: for humans and
// logs only, never parsed.
type WorkerError struct {
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Category string `json:"category,omitempty"`
	ErrorID  string `json:"errorId,omitempty"`
	// Exception is the .NET type of the exception.
	Exception string `json:"exception,omitempty"`
	HResult   int64  `json:"hresult,omitempty"`
}

// Error implements error.
func (e *WorkerError) Error() string {
	msg := e.Message
	if e.Category != "" || e.ErrorID != "" {
		msg += fmt.Sprintf(" (category %s, error %s)", e.Category, e.ErrorID)
	}
	return msg
}

// workerKind returns the kind of a *WorkerError in err's chain, or "".
func workerKind(err error) string {
	var we *WorkerError
	if errors.As(err, &we) {
		return we.Kind
	}
	return ""
}

// toStatus maps an error of a worker call to the error model
// (common.proto). Errors that already carry a status (from the supervisor:
// UNAVAILABLE while the worker restarts, DEADLINE_EXCEEDED on a timeout,
// the caller's context) pass through.
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch workerKind(err) {
	case kindVMMSDown:
		return status.Errorf(codes.Unavailable, "Hyper-V Virtual Machine Management service (vmms) is not running: %v", err)
	case "":
		return status.Errorf(codes.Internal, "%v", err)
	default:
		return status.Errorf(codes.Internal, "Hyper-V call failed: %v", err)
	}
}

// statusWithResult returns a status error carrying a ResizeResult in its
// details, as the error model asks for writes that touched the VM.
func statusWithResult(c codes.Code, before, after *agentv1.VM, format string, args ...any) error {
	st := status.Newf(c, format, args...)
	if d, err := st.WithDetails(&agentv1.ResizeResult{Before: before, After: after}); err == nil {
		st = d
	}
	return st.Err()
}

// refusalStatus maps a refusal from policy.Policy.Allows to its code
// (PERMISSION_DENIED or FAILED_PRECONDITION). Status errors pass through.
func refusalStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, policy.ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, policy.ErrFailedPrecondition):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return err
	}
}
