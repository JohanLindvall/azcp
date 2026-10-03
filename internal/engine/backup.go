package engine

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

func hasNumberedBackups(path string) bool {
	n, err := highestNumberedBackup(path)
	return err == nil && n.Sign() > 0
}

func nextNumbered(path string) (string, error) {
	n, err := highestNumberedBackup(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.~%s~", path, n.Add(n, big.NewInt(1))), nil
}

// highestNumberedBackup reads the directory and matches names by hand rather
// than globbing for them: the path being backed up may itself contain glob
// metacharacters ("report[1].pdf"), which filepath.Glob would interpret.
func highestNumberedBackup(path string) (*big.Int, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	highest := new(big.Int)
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), base+".~")
		if !ok {
			continue
		}
		num, ok := strings.CutSuffix(rest, "~")
		if !ok || num == "" || strings.IndexFunc(num, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			continue
		}
		if n, ok := new(big.Int).SetString(num, 10); ok && n.Cmp(highest) > 0 {
			highest = n
		}
	}
	return highest, nil
}
