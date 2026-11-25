package server

// PTYCapability describes how this node provides PTYs for the built-in SSH
// server. Mode is one of "conpty", "winpty", "pty" (Unix) or "unavailable".
type PTYCapability struct {
	Mode            string `json:"mode"`
	ConPTYAvailable bool   `json:"conptyAvailable"`
	AssetsPresent   bool   `json:"assetsPresent"`
	AssetsValid     bool   `json:"assetsValid"`
	Detail          string `json:"detail"`
}
