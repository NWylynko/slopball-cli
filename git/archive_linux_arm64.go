//go:build linux && arm64

package git

import _ "embed"

//go:embed bundled/linux-arm64.tar.xz
var embeddedArchive []byte
