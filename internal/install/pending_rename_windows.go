//go:build windows

package install

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Not run on Windows, the same as the rest of this package's Windows files:
// what is read and written is held by the tests of pending_rename.go, and the
// registry calls are the documented ones.

func init() {
	cancelRemovalAtReboot = cancelPendingDeletions
}

// pendingReadLimit is how many times the value is asked for again when it grew
// between asking for its size and reading it. Something else appending to it
// three times in that moment is something to report rather than to race.
const pendingReadLimit = 3

// cancelPendingDeletions takes out of PendingFileRenameOperations the
// deletions of the executable and of the data directory an install is about
// to use, and answers the paths whose deletions it took out.
//
// The value is read, filtered and written back, which is not one step: an
// installer that appends to it in between has its entry lost. The window is
// the few milliseconds between the read and the write, during an install an
// administrator started, and there is no call that removes one entry alone.
func cancelPendingDeletions(executable string, dataDir string) ([]string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, pendingRenameKey,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s to read the deletions at the next reboot: %w",
			pendingRenameKey, err)
	}

	defer func() {
		_ = key.Close()
	}()

	data, valueType, err := readRawValue(key, pendingRenameValue)

	switch {
	case errors.Is(err, registry.ErrNotExist):
		// Nothing is waiting for the next boot, which is the usual case.
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("failed to read %s: %w", pendingRenameValue, err)
	case valueType != registry.MULTI_SZ:
		// MoveFileEx writes nothing but REG_MULTI_SZ here, so a value of
		// another type holds none of the deletions an uninstall handed over,
		// and it is not this program's to rewrite.
		return nil, nil
	}

	values := splitMultiSZ(data)

	kept, dropped, err := dropDeletionsOf(values, executable, dataDir)
	if err != nil {
		return nil, err
	}

	if len(dropped) == 0 {
		return nil, nil
	}

	if len(kept) == 0 {
		// The session manager deletes the value once it has carried it out,
		// so no value is what a list with nothing left in it looks like.
		err = key.DeleteValue(pendingRenameValue)
		if err != nil {
			return nil, fmt.Errorf("failed to remove %s, which held nothing but the deletions of this "+
				"installation: %w", pendingRenameValue, err)
		}

		return dropped, nil
	}

	err = writeMultiSZ(key, pendingRenameValue, joinMultiSZ(kept))
	if err != nil {
		return nil, fmt.Errorf("failed to write %s back without the deletions of this installation: %w",
			pendingRenameValue, err)
	}

	return dropped, nil
}

// readRawValue reads a value as the UTF-16 it holds, with its type.
//
// registry.GetStringsValue is not used: it answers Go strings, and the
// strings of other programs are written back as they were read (see
// pending_rename.go).
func readRawValue(key registry.Key, name string) ([]uint16, uint32, error) {
	size, valueType, err := key.GetValue(name, nil)
	if err != nil {
		return nil, valueType, err
	}

	for try := 0; try < pendingReadLimit; try++ {
		buf := make([]byte, size+2)

		size, valueType, err = key.GetValue(name, buf)
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			continue
		}

		if err != nil {
			return nil, valueType, err
		}

		data := make([]uint16, size/2)
		for i := range data {
			data[i] = uint16(buf[2*i]) | uint16(buf[2*i+1])<<8
		}

		return data, valueType, nil
	}

	return nil, valueType, fmt.Errorf("%s kept growing while it was read", name)
}

// regSetValueEx is RegSetValueExW, which neither the windows package nor the
// registry package exports in a form that takes the bytes as they are.
var regSetValueEx = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegSetValueExW")

// writeMultiSZ writes data, which is already a whole REG_MULTI_SZ with its
// NULs, as the value name.
func writeMultiSZ(key registry.Key, name string, data []uint16) error {
	pointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}

	result, _, _ := regSetValueEx.Call(uintptr(key), uintptr(unsafe.Pointer(pointer)), 0,
		uintptr(registry.MULTI_SZ), uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	if result != 0 {
		return windows.Errno(result)
	}

	return nil
}
