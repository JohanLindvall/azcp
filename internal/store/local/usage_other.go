// SPDX-License-Identifier: MIT

//go:build !linux && !darwin && !windows

package local

import "io/fs"

func usageOf(string, fs.FileInfo) (Usage, error) { return Usage{}, nil }
