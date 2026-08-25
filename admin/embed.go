package admin

import "embed"

//go:embed templates/*
var templates embed.FS

//go:embed static/*
var static embed.FS

// GetStaticFS returns the embedded static file system for testing.
func GetStaticFS() embed.FS { return static }
