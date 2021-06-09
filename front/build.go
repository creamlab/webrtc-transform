package front

import (
	"log"
	"os"

	"github.com/evanw/esbuild/pkg/api"
)

var (
	developmentMode bool = false
	cmdBuildMode    bool = false
)

func init() {
	if os.Getenv("DS_ENV") == "DEV" {
		developmentMode = true
	}
	if os.Getenv("DS_ENV") == "BUILD_FRONT" {
		cmdBuildMode = true
	}
}

// API

func Build() {
	// only build in certain conditions (= not when launching ducksoup in production)
	if developmentMode || cmdBuildMode {
		buildOptions := api.BuildOptions{
			EntryPoints:       []string{"front/src/embed/app.js", "front/src/test_embed/app.js", "front/src/test_mirror/app.js", "front/src/test_standalone/app.js"},
			Bundle:            true,
			MinifyWhitespace:  true,
			MinifyIdentifiers: true,
			MinifySyntax:      true,
			Engines: []api.Engine{
				{api.EngineChrome, "64"},
				{api.EngineFirefox, "53"},
				{api.EngineSafari, "11"},
				{api.EngineEdge, "79"},
			},
			Outdir: "front/static/assets/scripts",
			Write:  true,
		}
		if developmentMode {
			buildOptions.Watch = &api.WatchMode{
				OnRebuild: func(result api.BuildResult) {
					if len(result.Errors) > 0 {
						log.Printf("watch build failed: %d errors\n", len(result.Errors))
						log.Println(result.Errors)
					} else {
						if len(result.Warnings) > 0 {
							log.Printf("watch build succeeded: %d warnings\n", len(result.Warnings))
							log.Println(result.Warnings)
						} else {
							log.Println("watch build succeeded")
						}
					}
				},
			}
		}
		build := api.Build(buildOptions)

		if len(build.Errors) > 0 {
			log.Fatal(build.Errors)
		}
	}
}
