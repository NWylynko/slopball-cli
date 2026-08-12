//go:build linux && amd64

package git

import _ "embed"

//go:embed bundled/linux-amd64.tar.xz
var embeddedArchive []byte
