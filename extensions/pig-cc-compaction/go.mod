module github.com/lynimbus/pig-cc-compaction

go 1.26

// The SDK version pig ships with. pig replaces it with the copy it staged at
// build time and test.sh replaces it with that same copy; pinning a real version
// keeps editors and plain builds working from the module cache.
require github.com/MichaelKinsy/PiG/extensions/sdk v0.7.0

require github.com/MoeForge/moejs v0.1.0

// moejs's own requirements, declared here because pig builds this package from a
// generated module that carries only what go.mod declares: without them the
// local build passes and the packed install fails with "updates to go.mod needed".
require (
	github.com/MoeForge/moego v0.1.0-alpha.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)
