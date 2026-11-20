package farm

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

func fsIdentity(f *os.File) (string, error) {
	var i windows.ByHandleFileInformation
	e := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &i)
	return fmt.Sprintf("%d:%d:%d", i.VolumeSerialNumber, i.FileIndexHigh, i.FileIndexLow), e
}
