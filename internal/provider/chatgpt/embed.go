package chatgpt

import _ "embed"

//go:embed bridge/main.js
var mainScriptTemplate string

//go:embed bridge/preload.js
var preloadScript []byte
