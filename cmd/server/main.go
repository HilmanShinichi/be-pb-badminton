package main

import (
	"github.com/pb-kecebong/backend/internal/app"
	"github.com/pb-kecebong/backend/internal/config"
)

func main() {
	config.LoadDotEnv(".env")
	app.Run(config.Load())
}
