//go:build !darwin && !linux

package platform

import "fmt"

func TerminalProcessGroup(fd int) (int, error) {
	return 0, fmt.Errorf("platform: terminal process groups are unsupported")
}

func SetTerminalProcessGroup(fd, pgid int) error {
	return fmt.Errorf("platform: terminal process groups are unsupported")
}
