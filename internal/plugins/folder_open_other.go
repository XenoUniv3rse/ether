//go:build !unix

package plugins

import "os"

const folderReadFlags = os.O_RDONLY
