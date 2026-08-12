package claudegen

import _ "embed"

//go:embed assets/package.json
var packageJSON []byte

//go:embed assets/package-lock.json
var packageLock []byte

//go:embed assets/bridge.mjs
var bridge []byte

func currentAssets() Assets {
	return Assets{PackageJSON: packageJSON, PackageLock: packageLock, Bridge: bridge}
}
