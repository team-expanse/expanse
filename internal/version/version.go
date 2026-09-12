package version

import (
	"encoding/json"
	"fmt"
	"runtime"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

func (i Info) String() string {
	return fmt.Sprintf("expanse %s (%s) %s %s", i.Version, i.Commit, i.Platform, i.GoVersion)
}

func (i Info) MarshalJSON() ([]byte, error) {
	type alias Info
	return json.Marshal((*alias)(&i))
}
