//go:build windows

package machineid

import (
	"errors"
	"fmt"
	"runtime"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows/registry"
)

const rpcEChangedMode = uintptr(0x80010106)

type windowsSource struct{}

// Current reads this Windows installation's hardware-backed identity sources.
func Current() (Result, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	shouldUninitialize, err := initializeCOM()
	if err != nil {
		return Result{}, fmt.Errorf("machine identity unavailable: initialize WMI: %w", err)
	}
	if shouldUninitialize {
		defer ole.CoUninitialize()
	}
	return Resolve(windowsSource{})
}

func initializeCOM() (bool, error) {
	err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	if err == nil {
		return true, nil
	}
	var oleErr *ole.OleError
	if errors.As(err, &oleErr) && oleErr.Code() == rpcEChangedMode {
		return false, nil
	}
	return false, err
}

func (windowsSource) ProcessorIDs() ([]string, error) {
	return queryWMIStrings("SELECT ProcessorId FROM Win32_Processor", "ProcessorId")
}

func (windowsSource) BaseBoardSerialNumbers() ([]string, error) {
	return queryWMIStrings("SELECT SerialNumber FROM Win32_BaseBoard", "SerialNumber")
}

func (windowsSource) MachineGUID() (string, error) {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return "", err
	}
	defer key.Close()
	value, _, err := key.GetStringValue("MachineGuid")
	return value, err
}

func queryWMIStrings(query, property string) ([]string, error) {
	unknown, err := oleutil.CreateObject("WbemScripting.SWbemLocator")
	if err != nil {
		return nil, err
	}
	defer unknown.Release()

	locator, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return nil, err
	}
	defer locator.Release()

	servicesVariant, err := oleutil.CallMethod(locator, "ConnectServer")
	if err != nil {
		return nil, err
	}
	defer servicesVariant.Clear()
	services := servicesVariant.ToIDispatch()
	if services == nil {
		return nil, errors.New("WMI ConnectServer returned no dispatch object")
	}

	objectsVariant, err := oleutil.CallMethod(services, "ExecQuery", query)
	if err != nil {
		return nil, err
	}
	defer objectsVariant.Clear()
	objects := objectsVariant.ToIDispatch()
	if objects == nil {
		return nil, errors.New("WMI ExecQuery returned no dispatch object")
	}

	values := make([]string, 0, 1)
	err = oleutil.ForEach(objects, func(item *ole.VARIANT) error {
		defer item.Clear()
		dispatch := item.ToIDispatch()
		if dispatch == nil {
			return errors.New("WMI query returned no dispatch object")
		}
		value, err := oleutil.GetProperty(dispatch, property)
		if err != nil {
			return err
		}
		defer value.Clear()
		if text, ok := value.Value().(string); ok {
			values = append(values, text)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}
