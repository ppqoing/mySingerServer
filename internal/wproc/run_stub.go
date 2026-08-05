//go:build !windows

package wproc

func Run(string, int) int {
	return 2
}
