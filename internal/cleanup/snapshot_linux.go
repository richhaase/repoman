package cleanup

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func checkNestedMounts(path string) error {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("cannot inspect nested mounts: %w", err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			return fmt.Errorf("cannot parse mount information")
		}
		mount := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
		if within(path, mount) {
			return fmt.Errorf("mounted filesystem at %q is protected", mount)
		}
	}
	return scanner.Err()
}
