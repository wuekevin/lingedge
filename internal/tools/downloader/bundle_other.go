//go:build !unix

package downloader

import "os"

const bundleOpenFlags = os.O_RDONLY
