package utils

import (
	"fmt"
	"runtime/debug"

	"github.com/module195/torrent-indexer/logging"
)

// RecoverAsError turns a panic in the calling goroutine into an error in
// *errp (when errp is not nil), and logs it with the stack. Defer it
// directly (defer utils.RecoverAsError(&err, "...")) in every goroutine that
// processes external data outside a net/http handler: there a panic is not
// recovered by the server and ends the whole process. Go only stops a panic
// when recover is called by the deferred function itself, so a deferred
// closure must call recover and pass the value to PanicError instead.
func RecoverAsError(errp *error, where string) {
	if r := recover(); r != nil {
		err := PanicError(r, where)
		if errp != nil {
			*errp = err
		}
	}
}

// PanicError logs a recovered panic value with the stack and returns it as
// an error.
func PanicError(r any, where string) error {
	logging.Error().Str("where", where).Str("stack", string(debug.Stack())).Msgf("Recovered panic: %v", r)
	return fmt.Errorf("panic in %s: %v", where, r)
}
