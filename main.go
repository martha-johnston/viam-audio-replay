package main

import (
	"audio-replay/models"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
)

func main() {
	// ModularMain can take multiple APIModel arguments, if your module implements multiple models.
	module.ModularMain(resource.APIModel{audioin.API, models.Audio})
}
