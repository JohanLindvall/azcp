package engine

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Renaming onto a backup name is a write too. It must not replace a file that
// an earlier operand has yet to open, or race a copy aimed at that name.
func (e *Engine) awaitBackup(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	u := &uri.URL{Scheme: uri.SchemeFile, Path: path}
	previous := e.scheduled[destinationKey(u)]
	if previous.sidecar {
		return plainf("cannot back up to %s: the name is reserved for another download's resume record", quote(path))
	}
	if previous.done != nil {
		if err := awaitTask(ctx, previous.done); err != nil {
			return err
		}
	}
	if info, err := os.Stat(path); err == nil {
		if id, _, ok := local.IDOf(path, info); ok {
			if _, err := e.awaitDestination(ctx, id); err != nil {
				return err
			}
			for _, reader := range e.localReads[id] {
				if err := awaitTask(ctx, reader); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

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
