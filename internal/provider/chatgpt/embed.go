package chatgpt

import _ "embed"

//go:embed bridge/main.js
var mainScriptTemplate string

//go:embed bridge/preload.js
var preloadScriptTemplate string

//go:embed bridge/navigation.js
var navigationScript []byte

//go:embed bridge/renderer-navigation.js
var rendererNavigationScript []byte

//go:embed bridge/ownership.js
var ownershipScript []byte

//go:embed bridge/capture.js
var captureScript []byte
