package assets

import "embed"

//go:embed static/* templates/*
var AssetsFS embed.FS
