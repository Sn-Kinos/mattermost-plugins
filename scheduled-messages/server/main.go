package main

import (
	// The server host may lack zoneinfo; without it Asia/Seoul would silently become UTC.
	_ "time/tzdata"

	"github.com/mattermost/mattermost/server/public/plugin"
)

func main() {
	plugin.ClientMain(&Plugin{})
}
