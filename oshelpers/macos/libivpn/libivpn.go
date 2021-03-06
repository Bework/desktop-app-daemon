package libivpn

/*
#include <libivpn.h>
*/
import (
	"C"
)

import (
	"ivpn/daemon/logger"
)

// TODO: reimplement accessing libivpn using syscall.NewLazyDLL+NewProc (avoid using CGO)

// Unload - unload (uninitialize\close) 'libivpn' shared library
func Unload() {
	C.UnLoadLibrary()
}

// StartXpcListener starts listener for helper
func StartXpcListener(tcpPort int) {

	ret := C.start_xpc_listener(C.CString("net.ivpn.client.Helper"), C.int(tcpPort))
	if ret == 0 {
		return
	}

	switch ret {
	case C.ERROR_LIB_NOT_FOUND:
		logger.Panic("Unable to find dynamic library")
	case C.ERROR_METHOD_NOT_FOUND:
		logger.Panic("Method was not found in dynamic library")
	default:
		logger.Panic("Unexpected error: ", ret)
	}
}
