//go:build windows

package local

import "context"

// repairStoreOnStartup does nothing on Windows. The local receiver does not
// run here (see errLocalUnsupported), so there is no store whose
// modification times a daemon could stamp.
func repairStoreOnStartup(context.Context, *Storage, *eventHub) {}
