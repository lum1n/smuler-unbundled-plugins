module github.com/lum1n/smuler/plugins/ci-github-actions

go 1.26

require (
	github.com/lum1n/smuler/plugins/httphealth v0.0.0
	github.com/lum1n/smuler/plugins/sdk-go v0.0.0
)

require github.com/lum1n/smuler/plugins/plugindebug v0.0.0 // indirect

replace (
	github.com/lum1n/smuler/plugins/httphealth => ../httphealth
	github.com/lum1n/smuler/plugins/plugindebug => ../plugindebug
	github.com/lum1n/smuler/plugins/sdk-go => ../sdk-go
)
